package oas

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/tjbrockmeyer/oasm"
	"github.com/tjbrockmeyer/vjsonschema"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

type EndpointDeclaration interface {
	// Set the version of this endpoint, updating the path to correspond to it
	Version(int) EndpointDeclaration
	// Set an arbitrary variable on this endpoint's Options object.
	Set(key string, value interface{}) EndpointDeclaration
	// Attach a parameter doc.
	// Valid 'in's are query, path, and header.
	// Valid 'kind's are String, Int, Float64, and Bool.
	Parameter(in, name, description string, required bool, schema interface{}, kind reflect.Kind) EndpointDeclaration
	// Attach a request body doc.
	// `schema` will be used in the documentation, and `object` will be used for reading the body automatically.
	RequestBody(description string, required bool, schema interface{}, object interface{}) EndpointDeclaration
	// Attach a response doc. Schema may be nil.
	Response(code int, description string, schema interface{}) EndpointDeclaration
	// Deprecate this endpoint.
	Deprecate(comment string) EndpointDeclaration
	// Attach a security doc.
	Security(nameToScopesMapping map[string][]string) EndpointDeclaration
	// Attach a function to run when calling this endpoint.
	// This should be the final function called when declaring an endpoint.
	// This will also create a large amount of metadata to be used when parsing a request.
	Define(f HandlerFunc) (Endpoint, error)
	// See: Define(f HandlerFunc) (Endpoint, error)
	// Panics if an error occurs.
	MustDefine(f HandlerFunc) Endpoint
}

type Endpoint interface {
	// The operation documentation.
	Doc() *oasm.Operation
	// Return the value of an option created using Set()
	Get(key string) interface{}
	// Return the method, path, and version of this endpoint (documentation that is not contained in Doc())
	Settings() (method, path string, version int)
	// Return the security requirements mapped to their corresponding security schemes.
	SecurityMapping() []map[string]oasm.SecurityScheme
	// The function that was defined by the user via Define()
	UserDefinedFunc(Data) (interface{}, error)
	// HTTP handler for the endpoint.
	Call(w http.ResponseWriter, r *http.Request)
}

type endpointObject struct {
	doc oasm.Operation
	err error

	path        string
	swaggerPath string
	method      string
	version     int
	regexPath   *regexp.Regexp

	options             map[string]interface{}
	userDefinedFunc     HandlerFunc
	fullyWrappedHandler http.Handler
	spec                *openAPI
	parsedPath          map[string]int

	bodyType       reflect.Type
	bodyJsonSchema json.RawMessage

	query   []typedParameter
	params  map[int]typedParameter
	headers []typedParameter

	reqSchemaName      string
	responseSchemaRefs map[int]string
}

type typedParameter struct {
	kind       reflect.Kind
	jsonSchema json.RawMessage
	oasm.Parameter
}

func (e *endpointObject) Version(version int) EndpointDeclaration {
	if version <= 0 || e.version != 0 {
		return e
	}
	v := fmt.Sprintf("/v%v", version)
	e.version = version
	e.doc.OperationId += v
	e.path = v + e.path
	e.parsePath()
	return e
}

func (e *endpointObject) Set(key string, value interface{}) EndpointDeclaration {
	e.options[key] = value
	return e
}

func (e *endpointObject) Parameter(in, name, description string, required bool, schema interface{}, kind reflect.Kind) EndpointDeclaration {
	param := oasm.Parameter{
		Name:        name,
		Description: description,
		In:          in,
		Required:    required,
		Schema:      schema,
	}
	if kind != reflect.String && kind != reflect.Int && kind != reflect.Float64 && kind != reflect.Bool {
		e.err = errors.New(
			fmt.Sprintf("invalid kind for parameter %s in %s: ", name, in) +
				"kind should be one of String, Int, Float64, Bool")
		return e
	}
	t := typedParameter{kind, nil, param}

	// Handle jsonschema and swagger schemas including references.
	b, err := json.Marshal(schema)
	if err != nil {
		e.err = errors.WithMessage(err, "failed to marshal parameter schema: "+e.doc.OperationId+" "+in+" "+name)
		return e
	}
	t.jsonSchema = b
	param.Schema = json.RawMessage(vjsonschema.SchemaRefReplace(b, refNameToSwaggerRef))
	e.doc.Parameters = append(e.doc.Parameters, param)

	// Handle go-type of the parameter
	switch in {
	case oasm.InQuery:
		e.query = append(e.query, t)
	case oasm.InPath:
		loc, ok := e.parsedPath[name]
		if !ok {
			e.printError(errors.New("path parameter provided in docs, but not provided in route"))
		} else {
			e.params[loc] = t
		}
	case oasm.InHeader:
		e.headers = append(e.headers, t)
	}
	return e
}

func (e *endpointObject) RequestBody(description string, required bool, schema, object interface{}) EndpointDeclaration {
	// Handle jsonschema and swagger schemas including references.
	b, err := json.Marshal(schema)
	if err != nil {
		e.err = errors.WithMessage(err, "failed to marshal request body schema: "+e.doc.OperationId)
		return e
	}
	e.bodyJsonSchema = b
	schema = json.RawMessage(vjsonschema.SchemaRefReplace(b, refNameToSwaggerRef))

	e.bodyType = reflect.TypeOf(object)
	e.doc.RequestBody = &oasm.RequestBody{
		Description: description,
		Required:    required,
		Content: oasm.MediaTypesMap{
			oasm.MimeJson: {
				Schema: schema,
			},
		},
	}
	return e
}

func (e *endpointObject) Response(code int, description string, schema interface{}) EndpointDeclaration {
	r := oasm.Response{
		Description: description,
	}

	if schema != nil {
		jsonSchemaRef := fmt.Sprint("endpoint_", e.doc.OperationId, "_response_", code)
		e.responseSchemaRefs[code] = jsonSchemaRef

		// Handle jsonschema and swagger schemas including references.
		b, err := json.Marshal(schema)
		if err != nil {
			e.err = errors.WithMessage(err, "failed to marshal response schema: "+fmt.Sprintf(e.doc.OperationId, code))
			return e
		}
		schema = json.RawMessage(vjsonschema.SchemaRefReplace(b, refNameToSwaggerRef))
		if err := e.spec.validatorBuilder.AddSchema(jsonSchemaRef, b); err != nil {
			e.err = errors.WithMessage(err, "failed to add response schema: "+fmt.Sprint(e.doc.OperationId, " ", code))
			return e
		}

		r.Content = oasm.MediaTypesMap{
			oasm.MimeJson: {
				Schema: schema,
			},
		}
	}
	e.doc.Responses.Codes[code] = r
	return e
}

func (e *endpointObject) Deprecate(comment string) EndpointDeclaration {
	e.doc.Deprecated = true
	if comment != "" {
		e.doc.Description += "<br/>DEPRECATED: " + comment
	}
	return e
}

func (e *endpointObject) Security(nameToScopesMapping map[string][]string) EndpointDeclaration {
	e.doc.Security = append(e.doc.Security, nameToScopesMapping)
	return e
}

func (e *endpointObject) MustDefine(f HandlerFunc) Endpoint {
	_, err := e.Define(f)
	if err != nil {
		panic(errors.WithMessage(err, "endpoint must define but failed"))
	}
	return e
}

func (e *endpointObject) Define(f HandlerFunc) (Endpoint, error) {
	if e.err != nil {
		return nil, e.err
	}
	var err error

	spec := e.spec

	e.userDefinedFunc = f

	// Create a schema for the data object.
	querySchema := map[string]interface{}{
		"type":       "object",
		"required":   make([]string, 0, 3),
		"properties": make(map[string]interface{}),
	}
	paramsSchema := map[string]interface{}{
		"type":       "object",
		"required":   make([]string, 0, 3),
		"properties": make(map[string]interface{}),
	}
	headersSchema := map[string]interface{}{
		"type":       "object",
		"required":   make([]string, 0, 3),
		"properties": make(map[string]interface{}),
	}

	dataSchema := map[string]interface{}{
		"type": "object",
		"required": []string{
			"Query",
			"Params",
			"Headers",
		},
		"properties": map[string]interface{}{
			"Query":   querySchema,
			"Params":  paramsSchema,
			"Headers": headersSchema,
		},
	}

	// Create schema for request body.
	doc := e.Doc()
	if doc.RequestBody != nil {
		dataSchema["properties"].(map[string]interface{})["Body"] = e.bodyJsonSchema
		if doc.RequestBody.Required {
			dataSchema["required"] = append(dataSchema["required"].([]string), "Body")
		}
	}

	// Create schema for a single parameter:
	addToSchema := func(addTo *map[string]interface{}, t typedParameter) {
		(*addTo)["properties"].(map[string]interface{})[t.Name] = t.jsonSchema
		if t.Required {
			(*addTo)["required"] = append((*addTo)["required"].([]string), t.Name)
		}
	}

	// Create schemas for all parameters
	for _, p := range e.query {
		addToSchema(&querySchema, p)
	}
	for _, p := range e.params {
		addToSchema(&paramsSchema, p)
	}
	for _, p := range e.headers {
		addToSchema(&headersSchema, p)
	}

	// Save the name of the schema for use in validations.
	dataSchemaBytes, err := json.Marshal(dataSchema)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to marshal data schema for: "+e.doc.OperationId)
	}
	if err = e.spec.validatorBuilder.AddSchema(e.reqSchemaName, dataSchemaBytes); err != nil {
		return nil, errors.WithMessage(err, "failed to add/parse data schema for: "+e.doc.OperationId)
	}
	if err = e.spec.buildValidator(); err != nil {
		return nil, err
	}

	// Create routes and docs for all endpoints
	pathItem, ok := spec.doc.Paths[e.swaggerPath]
	if !ok {
		pathItem = oasm.PathItem{
			Methods: make(map[string]oasm.Operation)}
		spec.doc.Paths[e.swaggerPath] = pathItem
	}
	pathItem.Methods[e.method] = *doc
	spec.routeCreator(e, http.HandlerFunc(e.Call))
	return e, nil
}

func (e *endpointObject) Doc() *oasm.Operation {
	return &e.doc
}

func (e *endpointObject) Settings() (method, path string, version int) {
	return e.method, e.path, e.version
}

func (e *endpointObject) Get(key string) interface{} {
	return e.options[key]
}

func (e *endpointObject) RegexPath() *regexp.Regexp {
	return e.regexPath
}

func (e *endpointObject) SecurityMapping() []map[string]oasm.SecurityScheme {
	schemes := make([]map[string]oasm.SecurityScheme, 0, 2)
	if e.spec.doc.Security != nil {
		for _, s := range e.spec.doc.Security {
			m := make(map[string]oasm.SecurityScheme)
			for name := range s {
				m[name] = e.spec.doc.Components.SecuritySchemes[name]
			}
			schemes = append(schemes, m)
		}
	}
	if e.doc.Security != nil {
		for _, s := range e.doc.Security {
			m := make(map[string]oasm.SecurityScheme)
			for name := range s {
				m[name] = e.spec.doc.Components.SecuritySchemes[name]
			}
			schemes = append(schemes, m)
		}
	}
	return schemes
}

func (e *endpointObject) UserDefinedFunc(d Data) (i interface{}, err error) {
	defer func() {
		panicErr := recover()
		if panicErr != nil {
			err = fmt.Errorf("a fatal error occurred: %v", panicErr)
			log.Printf("endpoint panic (%s %s): %s\n", e.method, e.swaggerPath, panicErr)
			debug.PrintStack()
		}
	}()
	if e.userDefinedFunc != nil {
		return e.userDefinedFunc(d)
	}
	return nil, errors.New("endpoint function is not defined for: " + e.doc.OperationId)
}

func (e *endpointObject) Call(w http.ResponseWriter, r *http.Request) {
	var (
		data   = NewData(w, r, e)
		output interface{}
		res    Response
	)

	endpointError := e.parseRequest(&data)
	if endpointError == nil {
		output, endpointError = e.UserDefinedFunc(data)
	}

	if endpointError != nil {
		if valErr, ok := endpointError.(jsonValidationError); ok {
			res = Response{
				Body:   valErr,
				Status: 400,
			}
			endpointError = nil
		} else if malErr, ok := endpointError.(malformedJSONError); ok {
			res = Response{
				Body:   malErr,
				Status: 400,
			}
			endpointError = nil
		} else {
			res = Response{
				Body:   "Internal Server Error",
				Status: 500,
			}
		}
	} else if response, ok := output.(Response); ok {
		if response.Ignore {
			return
		}
		res = response
	} else {
		res.Body = output
	}

	if res.Status == 0 {
		res.Status = 200
	}

	if res.Body == nil {
		w.WriteHeader(res.Status)
	} else {
		var b []byte
		var err error
		indent := e.spec.jsonIndent
		h := r.Header.Get(JSONIndentHeader)
		if h != "" {
			i, err2 := strconv.Atoi(h)
			if err2 != nil {
				e.printError(errors.WithMessagef(
					err2, `Expected header '%s' to be an integer or empty, found %s`, JSONIndentHeader, h))
			} else {
				indent = i
			}
		}
		if indent > 0 {
			b, err = json.MarshalIndent(res.Body, "", strings.Repeat(" ", indent))
		} else {
			b, err = json.Marshal(res.Body)
		}
		if err != nil {
			e.printError(errors.WithMessagef(err, "failed to marshal response body (%v)", res.Body))
			res.Status = 500
			b = []byte("Internal Server Error")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		if _, err = w.Write(b); err != nil {
			e.printError(errors.WithMessage(err, "error occurred while writing the response body"))
		}
	}

	// Validate response body
	if schema, ok := e.responseSchemaRefs[res.Status]; ok {
		bodyBytes, err := json.Marshal(res.Body)
		if err != nil {
			e.printError(errors.WithMessage(err, "failed to marshal response body"))
		} else {
			result, err := e.spec.validator.Validate(schema, bodyBytes)
			if err != nil {
				e.printError(errors.WithMessage(err, "response body contains malformed json"))
			} else if !result.Valid() {
				e.printError(errors.WithMessagef(
					newJSONValidationError(result), "response body failed validation for status %v", res.Status))
			}
		}
	}

	if e.spec.responseAndErrorHandler != nil {
		e.spec.responseAndErrorHandler(data, res, endpointError)
	} else {
		e.printError(endpointError)
	}
}

func (e *endpointObject) parseRequest(data *Data) error {
	var err error
	var requestBody []byte

	convertParamType := func(param typedParameter, item string) (interface{}, error) {
		switch param.kind {
		case reflect.String:
			return item, nil
		case reflect.Int:
			if i, err := strconv.Atoi(item); err != nil {
				return nil, newParameterTypeError(param.Parameter, "int", item)
			} else {
				return i, nil
			}
		case reflect.Float64:
			if i, err := strconv.ParseFloat(item, 64); err != nil {
				return nil, newParameterTypeError(param.Parameter, "float", item)
			} else {
				return i, nil
			}
		case reflect.Bool:
			if i, err := strconv.ParseBool(item); err != nil {
				return nil, newParameterTypeError(param.Parameter, "bool", item)
			} else {
				return i, nil
			}
		default:
			return nil, errors.New("bad reflection type for converting parameter from string")
		}
	}

	if e.bodyType != nil {
		requestBody, err = ioutil.ReadAll(data.Req.Body)
		if err != nil {
			return errors.WithMessage(err, "failed to read request body")
		}
		err = data.Req.Body.Close()
		if err != nil {
			return errors.WithMessage(err, "failed to close request body")
		}
	}

	if len(e.query) > 0 {
		getQueryParam := data.Req.URL.Query().Get
		for _, param := range e.query {
			name := param.Name
			query := getQueryParam(name)
			if query == "" {
				continue
			}
			data.Query[name], err = convertParamType(param, query)
			if err != nil {
				return errors.WithMessage(err, "failed to convert query parameter "+name)
			}
		}
	}

	if len(e.params) > 0 {
		log.Println(e.regexPath.String(), e.params)
		subMatches := e.regexPath.FindStringSubmatch(data.Req.URL.Path)
		log.Println(e.path, subMatches, e.regexPath.String(), data.Req.URL.Path)
		for loc, param := range e.params {
			data.Params[param.Name], err = convertParamType(param, subMatches[loc])
			if err != nil {
				return errors.WithMessage(err, "failed to convert path parameter "+param.Name)
			}
		}
	}

	if len(e.headers) > 0 {
		getHeader := data.Req.Header.Get
		for _, param := range e.headers {
			name := param.Name
			header := getHeader(name)
			if header == "" {
				continue
			}
			data.Headers[name], err = convertParamType(param, header)
			if err != nil {
				return errors.WithMessage(err, "failed to convert header parameter "+name)
			}
		}
	}

	dataJson := map[string]interface{}{
		"Query":   data.Query,
		"Params":  data.Params,
		"Headers": data.Headers,
	}
	if e.bodyType != nil {
		dataJson["Body"] = json.RawMessage(requestBody)
	}
	b, err := json.Marshal(dataJson)
	if err != nil {
		return newMalformedJSONError(err)
	}
	result, err := e.spec.validator.Validate(e.reqSchemaName, b)
	if err != nil {
		return newMalformedJSONError(err)
	}
	if !result.Valid() {
		return newJSONValidationError(result)
	}

	if e.bodyType != nil {
		data.Body = reflect.New(e.bodyType).Interface()
		err = json.Unmarshal(requestBody, data.Body)
		if err != nil {
			return newMalformedJSONError(err)
		}
	}
	return nil
}

func (e *endpointObject) printError(err error) {
	log.Printf("endpoint error (%s): %v\n", e.doc.OperationId, err)
}

func (e *endpointObject) parsePath() {
	var (
		newSwaggerPath = ""
		parsedPath     = make(map[string]int)

		pathComparison = ""
		pathRegexStr   = e.spec.url.Path
		pathParamIndex int
	)
	for _, subMatch := range pathRegex.FindAllStringSubmatch(e.path, -1) {
		pathComparison += subMatch[0]
		if subMatch[1] != "" {
			newSwaggerPath += "/{" + subMatch[1] + "}"
			pathParamIndex++
			parsedPath[subMatch[1]] = pathParamIndex
			if subMatch[2] != "" {
				pathRegexStr += "/(" + subMatch[2] + ")"
			} else {
				pathRegexStr += "/([^/]+)"
			}
		} else {
			newSwaggerPath += subMatch[0]
			pathRegexStr += subMatch[0]
		}
	}
	e.regexPath, e.err = regexp.Compile(pathRegexStr)
	if pathComparison != e.path {
		e.err = errors.New("endpoint path does not match the required format:\n" +
			e.path + "\n" + pathComparison + "\n" +
			"(ex: /abc/123/{id}/{other:[reg]*exp?} )")
	}
	e.swaggerPath = newSwaggerPath
	e.parsedPath = parsedPath
}
