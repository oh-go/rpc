// Copyright 2009 The Go Authors. All rights reserved.
// Copyright 2012 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rpc

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// ----------------------------------------------------------------------------
// Codec
// ----------------------------------------------------------------------------

// Codec creates a CodecRequest to process each request.
type Codec interface {
	NewRequest(*http.Request) CodecRequest
}

// CodecRequest decodes a request and encodes a response using a specific
// serialization scheme.
type CodecRequest interface {
	// Reads the request and returns the RPC method name.
	Method() (string, error)
	// Reads the request filling the RPC method args.
	ReadRequest(interface{}) error
	// Writes the response using the RPC method reply.
	WriteResponse(http.ResponseWriter, interface{})
	// Writes an error produced by the server.
	WriteError(w http.ResponseWriter, status int, err error)
}

// ----------------------------------------------------------------------------
// Server
// ----------------------------------------------------------------------------

// NewServer returns a new RPC server.
func NewServer() *Server {
	return &Server{
		codecs:   make(map[string]Codec),
		services: new(serviceMap),
	}
}

// RequestInfo contains all the information we pass to before/after functions
type RequestInfo struct {
	Method     string
	Error      error
	Request    *http.Request
	StatusCode int
}

// InterruptInfo contains
type InterruptInfo struct {
	Error      error
	StatusCode int
}

type InstrumentInfo struct {
	Duration   time.Duration
	Method     string
	StatusCode int
}

// Server serves registered RPC services using registered codecs.
type Server struct {
	codecs         map[string]Codec
	services       *serviceMap
	interruptFunc  func(i *RequestInfo) *InterruptInfo
	instrumentFunc func(i *InstrumentInfo)
}

// RegisterCodec adds a new codec to the server.
//
// Codecs are defined to process a given serialization scheme, e.g., JSON or
// XML. A codec is chosen based on the "Content-Type" header from the request,
// excluding the charset definition.
func (s *Server) RegisterCodec(codec Codec, contentType string) {
	s.codecs[strings.ToLower(contentType)] = codec
}

// RegisterService adds a new service to the server.
//
// The name parameter is optional: if empty it will be inferred from
// the receiver type name.
//
// Methods from the receiver will be extracted if these rules are satisfied:
//
//    - The receiver is exported (begins with an upper case letter) or local
//      (defined in the package registering the service).
//    - The method name is exported.
//    - The method has three arguments: *http.Request, *args, *reply.
//    - All three arguments are pointers.
//    - The second and third arguments are exported or local.
//    - The method has return type error.
//
// All other methods are ignored.
func (s *Server) RegisterService(receiver interface{}, name string) error {
	return s.services.register(receiver, name)
}

// HasMethod returns true if the given method is registered.
//
// The method uses a dotted notation as in "Service.Method".
func (s *Server) HasMethod(method string) bool {
	if _, _, err := s.services.get(method); err == nil {
		return true
	}
	return false
}

// RegisterInterruptFunc registers the specified function as the function
// that will be called before every request. The function is allowed to interrupt
// the request.
//
// Note: Only one function can be registered, subsequent calls to this
// method will overwrite all the previous functions.
func (s *Server) RegisterInterruptFunc(f func(i *RequestInfo) *InterruptInfo) {
	s.interruptFunc = f
}

// RegisterInstrumentFunc register the func which will give request info and handler process duration
func (s *Server) RegisterInstrumentFunc(f func(instrumentInfo *InstrumentInfo)) {
	s.instrumentFunc = f
}

// ServeHTTP
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var statusCode = 200

	if r.Method != "POST" {
		statusCode = 405
		WriteError(w, statusCode, "rpc: POST method required, received "+r.Method)
		return
	}
	contentType := r.Header.Get("Content-Type")
	idx := strings.Index(contentType, ";")
	if idx != -1 {
		contentType = contentType[:idx]
	}
	var codec Codec
	if contentType == "" && len(s.codecs) == 1 {
		// If Content-Type is not set and only one codec has been registered,
		// then default to that codec.
		for _, c := range s.codecs {
			codec = c
		}
	} else if codec = s.codecs[strings.ToLower(contentType)]; codec == nil {
		statusCode = 415
		WriteError(w, statusCode, "rpc: unrecognized Content-Type: "+contentType)
		return
	}
	// Create a new codec request.
	codecReq := codec.NewRequest(r)
	// Get service method to be called.
	method, errMethod := codecReq.Method()
	// Call the registered Intercept Function
	if s.interruptFunc != nil {
		interrupt := s.interruptFunc(&RequestInfo{
			Request: r,
			Method:  method,
		})
		if interrupt.Error != nil {
			codecReq.WriteError(w, interrupt.StatusCode, interrupt.Error)
			return
		}
	}

	defer func() { // call instrument func with method
		duration := time.Since(start)
		if s.instrumentFunc != nil {
			s.instrumentFunc(&InstrumentInfo{Method: method, Duration: duration, StatusCode: statusCode})
		}
	}()

	// method
	if errMethod != nil {
		statusCode = 400
		codecReq.WriteError(w, statusCode, errMethod)
		return
	}
	serviceSpec, methodSpec, errGet := s.services.get(method)
	if errGet != nil {
		statusCode = 400
		codecReq.WriteError(w, statusCode, errGet)
		return
	}
	// Decode the args.
	args := reflect.New(methodSpec.argsType)
	if errRead := codecReq.ReadRequest(args.Interface()); errRead != nil {
		statusCode = 400
		codecReq.WriteError(w, statusCode, errRead)
		return
	}
	// Call the service method.
	reply := reflect.New(methodSpec.replyType)
	errValue := methodSpec.method.Func.Call([]reflect.Value{
		serviceSpec.rcvr,
		reflect.ValueOf(r),
		args,
		reply,
	})
	// Cast the result to error if needed.
	var errResult error
	errInter := errValue[0].Interface()
	if errInter != nil {
		errResult = errInter.(error)
	}
	// Prevents Internet Explorer from MIME-sniffing a response away
	// from the declared content-type
	w.Header().Set("x-content-type-options", "nosniff")
	// Encode the response.
	if errResult == nil {
		codecReq.WriteResponse(w, reply.Interface())
	} else {
		statusCode = 400
		codecReq.WriteError(w, statusCode, errResult)
	}
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, msg)
}
