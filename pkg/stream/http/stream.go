/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package http

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/alipay/sofa-mosn/pkg/buffer"
	"github.com/alipay/sofa-mosn/pkg/log"
	"github.com/alipay/sofa-mosn/pkg/protocol"
	mosnhttp "github.com/alipay/sofa-mosn/pkg/protocol/http"
	str "github.com/alipay/sofa-mosn/pkg/stream"
	"github.com/alipay/sofa-mosn/pkg/types"
	"github.com/valyala/fasthttp"
)

var (
	errConnClose = errors.New("connection closed")

	HKConnection = []byte("Connection") // header key 'Connection'
	HVKeepAlive  = []byte("keep-alive") // header value 'keep-alive'
)

func init() {
	str.Register(protocol.HTTP1, &streamConnFactory{})
}

var (
	minMethodLengh = len("GET")
	maxMethodLengh = len("CONNECT")
	httpMethod     = map[string]struct{}{
		"OPTIONS": {},
		"GET":     {},
		"HEAD":    {},
		"POST":    {},
		"PUT":     {},
		"DELETE":  {},
		"TRACE":   {},
		"CONNECT": {},
	}
)

type streamConnFactory struct{}

func (f *streamConnFactory) CreateClientStream(context context.Context, connection types.ClientConnection,
	streamConnCallbacks types.StreamConnectionEventListener, connCallbacks types.ConnectionEventListener) types.ClientStreamConnection {
	return newClientStreamConnection(context, connection, streamConnCallbacks, connCallbacks)
}

func (f *streamConnFactory) CreateServerStream(context context.Context, connection types.Connection,
	callbacks types.ServerStreamConnectionEventListener) types.ServerStreamConnection {
	return newServerStreamConnection(context, connection, callbacks)
}

func (f *streamConnFactory) CreateBiDirectStream(context context.Context, connection types.ClientConnection,
	clientCallbacks types.StreamConnectionEventListener,
	serverCallbacks types.ServerStreamConnectionEventListener) types.ClientStreamConnection {
	return nil
}

func (f *streamConnFactory) ProtocolMatch(prot string, magic []byte) error {
	if len(magic) < minMethodLengh {
		return str.EAGAIN
	}
	size := len(magic)
	if len(magic) > maxMethodLengh {
		size = maxMethodLengh
	}

	for i := minMethodLengh; i <= size; i++ {
		if _, ok := httpMethod[string(magic[0:i])]; ok {
			return nil
		}
	}

	if size < maxMethodLengh {
		return str.EAGAIN
	} else {
		return str.FAILED
	}
}

// types.StreamConnection
// types.StreamConnectionEventListener
type streamConnection struct {
	context context.Context

	conn              types.Connection
	connEventListener types.ConnectionEventListener

	bufChan chan types.IoBuffer

	br *bufio.Reader
	bw *bufio.Writer

	logger log.Logger
}

// types.StreamConnection
func (conn *streamConnection) Dispatch(buffer types.IoBuffer) {
	for buffer.Len() > 0 {
		conn.bufChan <- buffer
		<-conn.bufChan
	}
}

func (conn *streamConnection) Protocol() types.Protocol {
	return protocol.HTTP1
}

func (conn *streamConnection) GoAway() {}

func (conn *streamConnection) Read(p []byte) (n int, err error) {
	data, ok := <-conn.bufChan

	// Connection close
	if !ok {
		err = errConnClose
		return
	}

	n = copy(p, data.Bytes())
	data.Drain(n)
	conn.bufChan <- nil
	return
}

func (conn *streamConnection) Write(p []byte) (n int, err error) {
	n = len(p)

	// TODO avoid copy
	buf := buffer.GetIoBuffer(n)
	buf.Write(p)

	err = conn.conn.Write(buf)
	return
}

// types.ClientStreamConnection
type clientStreamConnection struct {
	streamConnection

	stream                        *clientStream
	mutex                         sync.RWMutex
	connectionEventListener       types.ConnectionEventListener
	streamConnectionEventListener types.StreamConnectionEventListener
}

func newClientStreamConnection(context context.Context, connection types.ClientConnection,
	streamConnCallbacks types.StreamConnectionEventListener,
	connCallbacks types.ConnectionEventListener) types.ClientStreamConnection {

	csc := &clientStreamConnection{
		streamConnection: streamConnection{
			context: context,
			conn:    connection,
			bufChan: make(chan types.IoBuffer),
		},
		connectionEventListener:       connCallbacks,
		streamConnectionEventListener: streamConnCallbacks,
	}

	csc.br = bufio.NewReader(csc)
	csc.bw = bufio.NewWriter(csc)

	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.DefaultLogger.Errorf("http client serve goroutine panic %v", p)
				debug.PrintStack()

				csc.serve()
			}
		}()

		csc.serve()
	}()

	return csc
}

func (conn *clientStreamConnection) serve() {
	for {
		// 1. blocking read using fasthttp.Response.Read
		response := fasthttp.AcquireResponse()

		err := response.Read(conn.br)
		if err != nil {
			if conn.stream != nil {
				conn.stream.ResetStream(types.StreamRemoteReset)
				log.DefaultLogger.Errorf("Http client codec goroutine error: %s", err)
			}
			return
		}

		// 2. response processing
		s := conn.stream
		s.response = response

		resetConn := false
		if s.response.ConnectionClose() {
			resetConn = true
		}

		if atomic.LoadInt32(&s.readDisableCount) <= 0 {
			s.handleResponse()
		}

		// local reset
		if resetConn {
			// close connection
			s.connection.conn.Close(types.NoFlush, types.LocalClose)
			return
		}
	}
}

func (conn *clientStreamConnection) GoAway() {}

func (conn *clientStreamConnection) NewStream(ctx context.Context, receiver types.StreamReceiveListener) types.StreamSender {
	id := protocol.GenerateID()
	s := &clientStream{
		stream: stream{
			id:       id,
			ctx:      context.WithValue(ctx, types.ContextKeyStreamID, id),
			request:  fasthttp.AcquireRequest(),
			receiver: receiver,
		},
		connection: conn,
	}

	conn.mutex.Lock()
	conn.stream = s
	conn.mutex.Unlock()

	return s
}

func (conn *clientStreamConnection) ActiveStreamsNum() int {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()

	if conn.stream == nil {
		return 0
	} else {
		return 1
	}
}

func (conn *clientStreamConnection) Reset(reason types.StreamResetReason) {
	close(conn.bufChan)
}

// types.ServerStreamConnection
type serverStreamConnection struct {
	streamConnection

	stream                   *serverStream
	mutex                    sync.RWMutex
	serverStreamConnListener types.ServerStreamConnectionEventListener
}

func newServerStreamConnection(context context.Context, connection types.Connection,
	callbacks types.ServerStreamConnectionEventListener) types.ServerStreamConnection {
	ssc := &serverStreamConnection{
		streamConnection: streamConnection{
			context: context,
			conn:    connection,
			bufChan: make(chan types.IoBuffer),
		},
		serverStreamConnListener: callbacks,
	}

	ssc.br = bufio.NewReader(ssc)
	ssc.bw = bufio.NewWriter(ssc)

	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.DefaultLogger.Errorf("http server serve goroutine panic %v", p)
				debug.PrintStack()

				ssc.serve()
			}
		}()

		ssc.serve()
	}()

	return ssc
}

func (conn *serverStreamConnection) serve() {
	for {
		// 1. blocking read using fasthttp.Request.Read
		request := fasthttp.AcquireRequest()
		err := request.Read(conn.br)
		if err != nil {
			if conn.stream != nil {
				conn.stream.ResetStream(types.StreamRemoteReset)
				log.DefaultLogger.Errorf("Http server codec goroutine error: %s", err)
			}
			return
		}

		id := protocol.GenerateID()
		// 2. request processing
		s := &serverStream{
			stream: stream{
				id:       id,
				ctx:      context.WithValue(conn.context, types.ContextKeyStreamID, id),
				request:  request,
				response: fasthttp.AcquireResponse(),
			},
			connection:       conn,
			responseDoneChan: make(chan bool, 1),
		}

		s.receiver = conn.serverStreamConnListener.NewStreamDetect(s.stream.ctx, s, spanBuilder)

		conn.mutex.Lock()
		conn.stream = s
		conn.mutex.Unlock()

		if atomic.LoadInt32(&s.readDisableCount) <= 0 {
			s.handleRequest()
		}

		// wait for proxy done
		<-s.responseDoneChan
	}
}

func (conn *serverStreamConnection) ActiveStreamsNum() int {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()

	if conn.stream == nil {
		return 0
	} else {
		return 1
	}
}

func (conn *serverStreamConnection) Reset(reason types.StreamResetReason) {
	close(conn.bufChan)
}

// types.Stream
// types.StreamSender
type stream struct {
	str.BaseStream

	id               uint64
	readDisableCount int32
	ctx              context.Context

	// NOTICE: fasthttp ctx and its member not allowed holding by others after request handle finished
	request  *fasthttp.Request
	response *fasthttp.Response

	receiver types.StreamReceiveListener
}

// types.Stream
func (s *stream) ID() uint64 {
	return s.id
}

// types.StreamSender for request
type clientStream struct {
	stream

	connection *clientStreamConnection
}

// types.StreamSender
func (s *clientStream) AppendHeaders(context context.Context, headersIn types.HeaderMap, endStream bool) error {
	headers := headersIn.(mosnhttp.RequestHeader)

	// TODO: protocol convert in pkg/protocol
	//if the request contains body, use "POST" as default, the http request method will be setted by MosnHeaderMethod
	if endStream {
		headers.SetMethod(http.MethodGet)
	} else {
		headers.SetMethod(http.MethodPost)
	}

	removeInternalHeaders(headers, s.connection.conn.RemoteAddr())

	// copy headers
	headers.CopyTo(&s.request.Header)

	if endStream {
		s.endStream()
	}

	return nil
}

func (s *clientStream) AppendData(context context.Context, data types.IoBuffer, endStream bool) error {
	s.request.SetBody(data.Bytes())

	if endStream {
		s.endStream()
	}

	return nil
}

func (s *clientStream) AppendTrailers(context context.Context, trailers types.HeaderMap) error {
	s.endStream()
	return nil
}

func (s *clientStream) endStream() {
	s.doSend()
}

func (s *clientStream) ReadDisable(disable bool) {
	if disable {
		atomic.AddInt32(&s.readDisableCount, 1)
	} else {
		newCount := atomic.AddInt32(&s.readDisableCount, -1)

		if newCount <= 0 {
			s.handleResponse()
		}
	}
}

func (s *clientStream) doSend() {
	if _, err := s.request.WriteTo(s.connection); err != nil {
		log.DefaultLogger.Errorf("http1 client stream send error: %+s", err)
	}
}

func (s *clientStream) handleResponse() {
	if s.response != nil {
		header := mosnhttp.ResponseHeader{&s.response.Header, nil}

		statusCode := header.StatusCode()
		status := strconv.Itoa(statusCode)
		// inherit upstream's response status
		header.Set(types.HeaderStatus, status)

		log.DefaultLogger.Debugf("remote:%s, status:%s", s.connection.conn.RemoteAddr(), status)

		hasData := true
		if len(s.response.Body()) == 0 {
			hasData = false
		}

		s.connection.mutex.Lock()
		s.connection.stream = nil
		s.connection.mutex.Unlock()

		s.receiver.OnReceiveHeaders(s.ctx, header, !hasData)

		if hasData {
			s.receiver.OnReceiveData(s.ctx, buffer.NewIoBufferBytes(s.response.Body()), true)
		}

		//TODO cannot recycle immediately, headers might be used by proxy logic
		s.request = nil
		s.response = nil
	}
}

func (s *clientStream) GetStream() types.Stream {
	return s
}

// types.StreamSender for response
type serverStream struct {
	stream

	connection       *serverStreamConnection
	responseDoneChan chan bool
}

// types.StreamSender
func (s *serverStream) AppendHeaders(context context.Context, headersIn types.HeaderMap, endStream bool) error {
	switch headers := headersIn.(type) {
	case mosnhttp.RequestHeader:
		// hijack scene
		if status, ok := headers.Get(types.HeaderStatus); ok {
			headers.Del(types.HeaderStatus)

			statusCode, _ := strconv.Atoi(status)
			s.response.SetStatusCode(statusCode)

			removeInternalHeaders(headers, s.connection.conn.RemoteAddr())

			// need to echo all request headers for protocol convert
			headers.VisitAll(func(key, value []byte) {
				s.response.Header.SetBytesKV(key, value)
			})
		}
	case mosnhttp.ResponseHeader:
		if status, ok := headers.Get(types.HeaderStatus); ok {
			headers.Del(types.HeaderStatus)

			statusCode, _ := strconv.Atoi(status)
			headers.SetStatusCode(statusCode)
		}

		headers.CopyTo(&s.response.Header)
	}

	if endStream {
		s.endStream()
	}

	return nil
}

func (s *serverStream) AppendData(context context.Context, data types.IoBuffer, endStream bool) error {
	s.response.SetBody(data.Bytes())

	if endStream {
		s.endStream()
	}

	return nil
}

func (s *serverStream) AppendTrailers(context context.Context, trailers types.HeaderMap) error {
	s.endStream()
	return nil
}

func (s *serverStream) endStream() {
	resetConn := false
	// check if we need close connection
	if s.request.Header.ConnectionClose() {
		s.response.SetConnectionClose()
		resetConn = true
	} else if !s.request.Header.IsHTTP11() {
		// Set 'Connection: keep-alive' response header for non-HTTP/1.1 request.
		// There is no need in setting this header for http/1.1, since in http/1.1
		// connections are keep-alive by default.
		s.response.Header.SetCanonical(HKConnection, HVKeepAlive)
	}
	defer s.DestroyStream()

	s.doSend()
	s.responseDoneChan <- true

	if resetConn {
		// close connection
		s.connection.conn.Close(types.FlushWrite, types.LocalClose)
	}

	// clean up & recycle
	s.connection.mutex.Lock()
	s.connection.stream = nil
	s.connection.mutex.Unlock()

	if s.request != nil {
		fasthttp.ReleaseRequest(s.request)
	}
	if s.response != nil {
		fasthttp.ReleaseResponse(s.response)
	}
}

func (s *serverStream) ReadDisable(disable bool) {
	if disable {
		atomic.AddInt32(&s.readDisableCount, 1)
	} else {
		newCount := atomic.AddInt32(&s.readDisableCount, -1)

		if newCount <= 0 {
			s.handleRequest()
		}
	}
}

func (s *serverStream) doSend() {
	s.response.WriteTo(s.connection)
}

func (s *serverStream) handleRequest() {
	if s.request != nil {

		// header
		header := mosnhttp.RequestHeader{&s.request.Header, nil}

		// set non-header info in request-line, like method, uri
		injectInternalHeaders(header, s.request.URI())

		hasData := true
		if len(s.request.Body()) == 0 {
			hasData = false
		}
		s.receiver.OnReceiveHeaders(s.ctx, header, !hasData)

		if hasData {
			s.receiver.OnReceiveData(s.ctx, buffer.NewIoBufferBytes(s.request.Body()), true)
		}
	}
}

func (s *serverStream) GetStream() types.Stream {
	return s
}

// consider host, method, path are necessary, but check querystring
func injectInternalHeaders(headers mosnhttp.RequestHeader, uri *fasthttp.URI) {
	// 1. host
	headers.Set(protocol.MosnHeaderHostKey, string(uri.Host()))
	// 2. :authority
	headers.Set(protocol.IstioHeaderHostKey, string(uri.Host()))
	// 3. method
	headers.Set(protocol.MosnHeaderMethod, string(headers.Method()))
	// 4. path
	headers.Set(protocol.MosnHeaderPathKey, string(uri.Path()))
	// 5. querystring
	qs := uri.QueryString()
	if len(qs) > 0 {
		headers.Set(protocol.MosnHeaderQueryStringKey, string(qs))
	}
}

func removeInternalHeaders(headers mosnhttp.RequestHeader, remoteAddr net.Addr) {
	// assemble uri
	uri := ""

	// path
	if path, ok := headers.Get(protocol.MosnHeaderPathKey); ok && path != "" {
		headers.Del(protocol.MosnHeaderPathKey)
		uri += path
	} else {
		uri += "/"
	}

	// querystring
	queryString, ok := headers.Get(protocol.MosnHeaderQueryStringKey)
	if ok && queryString != "" {
		headers.Del(protocol.MosnHeaderQueryStringKey)
		uri += "?" + queryString
	}

	headers.SetRequestURI(uri)

	if method, ok := headers.Get(protocol.MosnHeaderMethod); ok {
		headers.Del(protocol.MosnHeaderMethod)
		headers.SetMethod(method)
	}

	if host, ok := headers.Get(protocol.MosnHeaderHostKey); ok {
		headers.Del(protocol.MosnHeaderHostKey)
		headers.SetHost(host)
	} else {
		headers.SetHost(remoteAddr.String())
	}

	if host, ok := headers.Get(protocol.IstioHeaderHostKey); ok {
		headers.Del(protocol.IstioHeaderHostKey)
		headers.SetHost(host)
	}

}
