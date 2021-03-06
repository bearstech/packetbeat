package http

import (
	"bytes"
	"errors"
	"fmt"
	"packetbeat/common"
	"packetbeat/config"
	"packetbeat/logp"
	"packetbeat/procs"
	"packetbeat/protos"
	"packetbeat/protos/tcp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	START = iota
	FLINE
	HEADERS
	BODY
	BODY_CHUNKED_START
	BODY_CHUNKED
	BODY_CHUNKED_WAIT_FINAL_CRLF
)

// Http Message
type HttpMessage struct {
	Ts               time.Time
	hasContentLength bool
	headerOffset     int
	bodyOffset       int
	version_major    uint8
	version_minor    uint8
	connection       string
	chunked_length   int
	chunked_body     []byte

	IsRequest    bool
	TcpTuple     common.TcpTuple
	CmdlineTuple *common.CmdlineTuple
	Direction    uint8
	//Request Info
	FirstLine    string
	RequestUri   string
	Method       string
	StatusCode   uint16
	StatusPhrase string
	Real_ip      string
	// Http Headers
	ContentLength    int
	TransferEncoding string
	Headers          map[string]string
	Body             string
	//Raw Data
	Raw []byte
	//Timing
	start int
	end   int
}

type HttpStream struct {
	tcptuple *common.TcpTuple

	data []byte

	parseOffset  int
	parseState   int
	bodyReceived int

	message *HttpMessage
}

type HttpTransaction struct {
	Type         string
	tuple        common.TcpTuple
	Src          common.Endpoint
	Dst          common.Endpoint
	Real_ip      string
	ResponseTime int32
	Ts           int64
	JsTs         time.Time
	ts           time.Time
	cmdline      *common.CmdlineTuple
	Method       string
	RequestUri   string

	Http common.MapStr

	Request_raw  string
	Response_raw string

	timer *time.Timer
}

type Http struct {
	// config
	Send_request      bool
	Send_response     bool
	Send_headers      bool
	Send_all_headers  bool
	Headers_whitelist map[string]bool
	Split_cookie      bool
	Real_ip_header    string

	transactionsMap map[common.HashableTcpTuple]*HttpTransaction

	results chan common.MapStr
}

func (http *Http) InitDefaults() {
	http.Send_request = true
	http.Send_response = true
}

func (http *Http) SetFromConfig(config *config.Config, meta *toml.MetaData) (err error) {
	if meta.IsDefined("protocols", "http", "send_request") {
		http.Send_request = config.Protocols["http"].Send_request
	}
	if meta.IsDefined("protocols", "http", "send_response") {
		http.Send_response = config.Protocols["http"].Send_response
	}

	if config.Http.Send_all_headers {
		http.Send_headers = true
		http.Send_all_headers = true
	} else {
		if len(config.Http.Send_headers) > 0 {
			http.Send_headers = true

			http.Headers_whitelist = map[string]bool{}
			for _, hdr := range config.Http.Send_headers {
				http.Headers_whitelist[strings.ToLower(hdr)] = true
			}
		}
	}

	http.Split_cookie = config.Http.Split_cookie

	http.Real_ip_header = strings.ToLower(config.Http.Real_ip_header)

	return nil
}

const (
	TransactionsHashSize = 2 ^ 16
	TransactionTimeout   = 10 * 1e9
)

func (http *Http) Init(test_mode bool, results chan common.MapStr) error {

	http.InitDefaults()

	if !test_mode {
		err := http.SetFromConfig(&config.ConfigSingleton, &config.ConfigMeta)
		if err != nil {
			return err
		}
	}

	http.transactionsMap = make(map[common.HashableTcpTuple]*HttpTransaction, TransactionsHashSize)

	logp.Debug("http", "transactionsMap: %p http: %p", http.transactionsMap, &http)

	http.results = results

	return nil
}

func parseVersion(s []byte) (uint8, uint8, error) {
	if len(s) < 3 {
		return 0, 0, errors.New("Invalid version")
	}

	major, _ := strconv.Atoi(string(s[0]))
	minor, _ := strconv.Atoi(string(s[2]))

	return uint8(major), uint8(minor), nil
}

func parseResponseStatus(s []byte) (uint16, string, error) {

	logp.Debug("http", "parseResponseStatus: %s", s)

	p := bytes.Index(s, []byte(" "))
	if p == -1 {
		return 0, "", errors.New("Not beeing able to identify status code")
	}

	status_code, _ := strconv.Atoi(string(s[0:p]))

	p = bytes.LastIndex(s, []byte(" "))
	if p == -1 {
		return uint16(status_code), "", errors.New("Not beeing able to identify status code")
	}
	status_phrase := s[p+1:]
	return uint16(status_code), string(status_phrase), nil
}

func (http *Http) parseHeader(m *HttpMessage, data []byte) (bool, bool, int) {
	if m.Headers == nil {
		m.Headers = make(map[string]string)
	}
	i := bytes.Index(data, []byte(":"))
	if i == -1 {
		// Expected \":\" in headers. Assuming incomplete"
		return true, false, 0
	}

	logp.Debug("httpdetailed", "Data: %s", data)
	logp.Debug("httpdetailed", "Header: %s", data[:i])

	// skip folding line
	for p := i + 1; p < len(data); {
		q := bytes.Index(data[p:], []byte("\r\n"))
		if q == -1 {
			// Assuming incomplete
			return true, false, 0
		}
		p += q
		logp.Debug("httpdetailed", "HV: %s\n", data[i+1:p])
		if len(data) > p && (data[p+1] == ' ' || data[p+1] == '\t') {
			p = p + 2
		} else {
			headerName := strings.ToLower(string(data[:i]))
			headerVal := string(bytes.Trim(data[i+1:p], " \t"))
			logp.Debug("http", "Header: '%s' Value: '%s'\n", headerName, headerVal)

			// Headers we need for parsing. Make sure we always
			// capture their value
			if headerName == "content-length" {
				m.ContentLength, _ = strconv.Atoi(headerVal)
				m.hasContentLength = true
			} else if headerName == "transfer-encoding" {
				m.TransferEncoding = headerVal
			} else if headerName == "connection" {
				m.connection = headerVal
			}
			if len(http.Real_ip_header) > 0 && headerName == http.Real_ip_header {
				m.Real_ip = headerVal
			}

			if http.Send_headers {
				if !http.Send_all_headers {
					_, exists := http.Headers_whitelist[headerName]
					if !exists {
						return true, true, p + 2
					}
				}
				if val, ok := m.Headers[headerName]; ok {
					m.Headers[headerName] = val + ", " + headerVal
				} else {
					m.Headers[headerName] = headerVal
				}
			}

			return true, true, p + 2
		}
	}

	return true, false, len(data)
}

func (http *Http) messageParser(s *HttpStream) (bool, bool) {

	var cont, ok, complete bool
	m := s.message

	logp.Debug("http", "Stream state=%d", s.parseState)

	for s.parseOffset < len(s.data) {
		switch s.parseState {
		case START:
			m.start = s.parseOffset
			i := bytes.Index(s.data[s.parseOffset:], []byte("\r\n"))
			if i == -1 {
				return true, false
			}

			// Very basic tests on the first line. Just to check that
			// we have what looks as an HTTP message
			var version []byte
			var err error
			fline := s.data[s.parseOffset:i]
			if len(fline) < 8 {
				logp.Debug("http", "First line too small")
				return false, false
			}
			if bytes.Equal(fline[0:5], []byte("HTTP/")) {
				//RESPONSE
				m.IsRequest = false
				version = fline[5:8]
				m.StatusCode, m.StatusPhrase, err = parseResponseStatus(fline[9:])
				if err != nil {
					logp.Warn("Failed to understand HTTP response status: %s", fline[9:])
					return false, false
				}
				logp.Debug("http", "HTTP status_code=%d, status_phrase=%s", m.StatusCode, m.StatusPhrase)

			} else {
				// REQUEST
				slices := bytes.Fields(fline)
				if len(slices) != 3 {
					logp.Debug("http", "Couldn't understand HTTP request: %s", fline)
					return false, false
				}

				m.Method = string(slices[0])
				m.RequestUri = string(slices[1])

				if bytes.Equal(slices[2][:5], []byte("HTTP/")) {
					m.IsRequest = true
					version = slices[2][5:]
					m.FirstLine = string(fline)
				} else {
					logp.Debug("http", "Couldn't understand HTTP version: %s", fline)
					return false, false
				}
				logp.Debug("http", "HTTP Method=%s, RequestUri=%s", m.Method, m.RequestUri)
			}

			m.version_major, m.version_minor, err = parseVersion(version)
			if err != nil {
				logp.Debug("http", "Failed to understand HTTP version: %s", version)
				m.version_major = 1
				m.version_minor = 0
			}
			logp.Debug("http", "HTTP version %d.%d", m.version_major, m.version_minor)

			// ok so far
			s.parseOffset = i + 2
			m.headerOffset = s.parseOffset
			s.parseState = HEADERS

		case HEADERS:

			if len(s.data)-s.parseOffset >= 2 &&
				bytes.Equal(s.data[s.parseOffset:s.parseOffset+2], []byte("\r\n")) {
				// EOH
				s.parseOffset += 2
				m.bodyOffset = s.parseOffset
				if !m.IsRequest && ((100 <= m.StatusCode && m.StatusCode < 200) || m.StatusCode == 204 || m.StatusCode == 304) {
					//response with a 1xx, 204 , or 304 status  code is always terminated
					// by the first empty line after the  header fields
					logp.Debug("http", "Terminate response, status code %d", m.StatusCode)
					m.end = s.parseOffset
					return true, true
				}
				if m.TransferEncoding == "chunked" {
					// support for HTTP/1.1 Chunked transfer
					// Transfer-Encoding overrides the Content-Length
					logp.Debug("http", "Read chunked body")
					s.parseState = BODY_CHUNKED_START
					continue
				}
				if m.ContentLength == 0 && (m.IsRequest || m.hasContentLength) {
					logp.Debug("http", "Empty content length, ignore body")
					// Ignore body for request that contains a message body but not a Content-Length
					m.end = s.parseOffset
					return true, true
				}
				logp.Debug("http", "Read body")
				s.parseState = BODY
			} else {
				ok, hfcomplete, offset := http.parseHeader(m, s.data[s.parseOffset:])

				if !ok {
					return false, false
				}
				if !hfcomplete {
					return true, false
				}
				s.parseOffset += offset
			}

		case BODY:
			logp.Debug("http", "eat body: %d", s.parseOffset)
			if !m.hasContentLength && (m.connection == "close" || (m.version_major == 1 && m.version_minor == 0 && m.connection != "keep-alive")) {
				// HTTP/1.0 no content length. Add until the end of the connection
				logp.Debug("http", "close connection, %d", len(s.data)-s.parseOffset)
				s.bodyReceived += (len(s.data) - s.parseOffset)
				m.ContentLength += (len(s.data) - s.parseOffset)
				s.parseOffset = len(s.data)
				return true, false
			} else if len(s.data[s.parseOffset:]) >= m.ContentLength-s.bodyReceived {
				s.parseOffset += (m.ContentLength - s.bodyReceived)
				m.end = s.parseOffset
				return true, true
			} else {
				s.bodyReceived += (len(s.data) - s.parseOffset)
				s.parseOffset = len(s.data)
				logp.Debug("http", "bodyReceived: %d", s.bodyReceived)
				return true, false
			}

		case BODY_CHUNKED_START:
			cont, ok, complete = state_body_chunked_start(s, m)
			if !cont {
				return ok, complete
			}

		case BODY_CHUNKED:
			cont, ok, complete = state_body_chunked(s, m)
			if !cont {
				return ok, complete
			}

		case BODY_CHUNKED_WAIT_FINAL_CRLF:
			return state_body_chunked_wait_final_crlf(s, m)
		}

	}

	return true, false
}

func state_body_chunked_wait_final_crlf(s *HttpStream, m *HttpMessage) (ok bool, complete bool) {
	if len(s.data[s.parseOffset:]) < 2 {
		return true, false
	} else {
		if s.data[s.parseOffset] != '\r' || s.data[s.parseOffset+1] != '\n' {
			logp.Warn("Expected CRLF sequence at end of message")
			return false, false
		}
		s.parseOffset += 2 // skip final CRLF
		m.end = s.parseOffset
		return true, true
	}
}

func state_body_chunked_start(s *HttpStream, m *HttpMessage) (cont bool, ok bool, complete bool) {
	// read hexa length
	i := bytes.Index(s.data[s.parseOffset:], []byte("\r\n"))
	if i == -1 {
		return false, true, false
	}
	line := string(s.data[s.parseOffset : s.parseOffset+i])
	_, err := fmt.Sscanf(line, "%x", &m.chunked_length)
	if err != nil {
		logp.Warn("Failed to understand chunked body start line")
		return false, false, false
	}

	s.parseOffset += i + 2 //+ \r\n
	if m.chunked_length == 0 {
		if len(s.data[s.parseOffset:]) < 2 {
			s.parseState = BODY_CHUNKED_WAIT_FINAL_CRLF
			return false, true, false
		}
		if s.data[s.parseOffset] != '\r' || s.data[s.parseOffset+1] != '\n' {
			logp.Warn("Expected CRLF sequence at end of message")
			return false, false, false
		}
		s.parseOffset += 2 // skip final CRLF

		m.end = s.parseOffset
		return false, true, true
	}
	s.bodyReceived = 0
	s.parseState = BODY_CHUNKED

	return true, true, false
}

func state_body_chunked(s *HttpStream, m *HttpMessage) (cont bool, ok bool, complete bool) {

	if len(s.data[s.parseOffset:]) >= m.chunked_length-s.bodyReceived+2 /*\r\n*/ {
		// Received more data than expected
		m.chunked_body = append(m.chunked_body, s.data[s.parseOffset:s.parseOffset+m.chunked_length-s.bodyReceived]...)
		s.parseOffset += (m.chunked_length - s.bodyReceived + 2 /*\r\n*/)
		m.ContentLength += m.chunked_length
		s.parseState = BODY_CHUNKED_START
		return true, true, false
	} else {
		if len(s.data[s.parseOffset:]) >= m.chunked_length-s.bodyReceived {
			// we need need to wait for the +2, else we can crash on next call
			return false, true, false
		}
		// Received less data than expected
		m.chunked_body = append(m.chunked_body, s.data[s.parseOffset:]...)
		s.bodyReceived += (len(s.data) - s.parseOffset)
		s.parseOffset = len(s.data)
		return false, true, false
	}
}

func (stream *HttpStream) PrepareForNewMessage() {
	stream.data = stream.data[stream.message.end:]
	stream.parseState = START
	stream.parseOffset = 0
	stream.bodyReceived = 0
	stream.message = nil
}

type httpPrivateData struct {
	Data [2]*HttpStream
}

func (http *Http) Parse(pkt *protos.Packet, tcptuple *common.TcpTuple,
	dir uint8, private protos.ProtocolData) protos.ProtocolData {

	defer logp.Recover("ParseHttp exception")

	logp.Debug("httpdetailed", "Payload received: [%s]", pkt.Payload)

	priv := httpPrivateData{}
	if private != nil {
		var ok bool
		priv, ok = private.(httpPrivateData)
		if !ok {
			priv = httpPrivateData{}
		}
	}

	if priv.Data[dir] == nil {
		priv.Data[dir] = &HttpStream{
			tcptuple: tcptuple,
			data:     pkt.Payload,
			message:  &HttpMessage{Ts: pkt.Ts},
		}

	} else {
		// concatenate bytes
		priv.Data[dir].data = append(priv.Data[dir].data, pkt.Payload...)
		if len(priv.Data[dir].data) > tcp.TCP_MAX_DATA_IN_STREAM {
			logp.Debug("http", "Stream data too large, dropping TCP stream")
			priv.Data[dir] = nil
			return priv
		}
	}
	stream := priv.Data[dir]
	if stream.message == nil {
		stream.message = &HttpMessage{Ts: pkt.Ts}
	}
	ok, complete := http.messageParser(stream)

	if !ok {
		// drop this tcp stream. Will retry parsing with the next
		// segment in it
		priv.Data[dir] = nil
		return priv
	}

	if complete {
		// all ok, ship it
		msg := stream.data[stream.message.start:stream.message.end]
		http.censorPasswords(stream.message, msg)

		http.handleHttp(stream.message, tcptuple, dir, msg)

		// and reset message
		stream.PrepareForNewMessage()
	}

	return priv
}

func (http *Http) ReceivedFin(tcptuple *common.TcpTuple, dir uint8,
	private protos.ProtocolData) protos.ProtocolData {

	if private == nil {
		return private
	}
	httpData, ok := private.(httpPrivateData)
	if !ok {
		return private
	}
	if httpData.Data[dir] == nil {
		return httpData
	}

	stream := httpData.Data[dir]

	// send whatever data we got so far as complete. This
	// is needed for the HTTP/1.0 without Content-Length situation.
	if stream.message != nil &&
		len(stream.data[stream.message.start:]) > 0 {

		logp.Debug("httpdetailed", "Publish something on connection FIN")

		msg := stream.data[stream.message.start:]
		http.censorPasswords(stream.message, msg)

		http.handleHttp(stream.message, tcptuple, dir, msg)

		// and reset message. Probably not needed, just to be sure.
		stream.PrepareForNewMessage()
	}

	return httpData
}

func (http *Http) GapInStream(tcptuple *common.TcpTuple, dir uint8,
	private protos.ProtocolData) protos.ProtocolData {

	return private
}

func (http *Http) handleHttp(m *HttpMessage, tcptuple *common.TcpTuple,
	dir uint8, raw_msg []byte) {

	m.TcpTuple = *tcptuple
	m.Direction = dir
	m.CmdlineTuple = procs.ProcWatcher.FindProcessesTuple(tcptuple.IpPort())
	m.Raw = raw_msg

	if m.IsRequest {
		http.receivedHttpRequest(m)
	} else {
		http.receivedHttpResponse(m)
	}
}

func (http *Http) receivedHttpRequest(msg *HttpMessage) {

	trans := http.transactionsMap[msg.TcpTuple.Hashable()]
	if trans != nil {
		if len(trans.Http) != 0 {
			logp.Warn("Two requests without a response. Dropping old request")
		}
	} else {
		trans = &HttpTransaction{Type: "http", tuple: msg.TcpTuple}
		logp.Debug("http", "transactionsMap %p http %p", http.transactionsMap, http)
		http.transactionsMap[msg.TcpTuple.Hashable()] = trans
	}

	logp.Debug("http", "Received request with tuple: %s", msg.TcpTuple)

	trans.ts = msg.Ts
	trans.Ts = int64(trans.ts.UnixNano() / 1000)
	trans.JsTs = msg.Ts
	trans.Src = common.Endpoint{
		Ip:   msg.TcpTuple.Src_ip.String(),
		Port: msg.TcpTuple.Src_port,
		Proc: string(msg.CmdlineTuple.Src),
	}
	trans.Dst = common.Endpoint{
		Ip:   msg.TcpTuple.Dst_ip.String(),
		Port: msg.TcpTuple.Dst_port,
		Proc: string(msg.CmdlineTuple.Dst),
	}
	if msg.Direction == tcp.TcpDirectionReverse {
		trans.Src, trans.Dst = trans.Dst, trans.Src
	}

	// save Raw message
	if http.Send_request {
		trans.Request_raw = string(http.cutMessageBody(msg))
	}

	trans.Method = msg.Method
	trans.RequestUri = msg.RequestUri

	trans.Http = common.MapStr{}

	if http.Send_headers {
		if !http.Split_cookie {
			trans.Http["request_headers"] = msg.Headers
		} else {
			hdrs := common.MapStr{}
			for hdr_name, hdr_val := range msg.Headers {
				if hdr_name == "cookie" {
					hdrs[hdr_name] = splitCookiesHeader(hdr_val)
				} else {
					hdrs[hdr_name] = hdr_val
				}
			}

			trans.Http["request_headers"] = hdrs
		}
	}

	trans.Real_ip = msg.Real_ip

	if trans.timer != nil {
		trans.timer.Stop()
	}
	trans.timer = time.AfterFunc(TransactionTimeout, func() { http.expireTransaction(trans) })

}

func (http *Http) expireTransaction(trans *HttpTransaction) {
	// remove from map
	delete(http.transactionsMap, trans.tuple.Hashable())
}

func (http *Http) receivedHttpResponse(msg *HttpMessage) {

	// we need to search the request first.
	tuple := msg.TcpTuple

	logp.Debug("http", "Received response with tuple: %s", tuple)

	trans := http.transactionsMap[tuple.Hashable()]
	if trans == nil {
		logp.Warn("Response from unknown transaction. Ignoring: %v", tuple)
		return
	}

	if trans.Http == nil {
		logp.Warn("Response without a known request. Ignoring.")
		return
	}

	response := common.MapStr{
		"phrase":         msg.StatusPhrase,
		"code":           msg.StatusCode,
		"content_length": msg.ContentLength,
	}

	if http.Send_headers {
		if !http.Split_cookie {
			response["response_headers"] = msg.Headers
		} else {
			hdrs := common.MapStr{}
			for hdr_name, hdr_val := range msg.Headers {
				if hdr_name == "set-cookie" {
					hdrs[hdr_name] = splitCookiesHeader(hdr_val)
				} else {
					hdrs[hdr_name] = hdr_val
				}
			}

			response["response_headers"] = hdrs
		}
	}

	trans.Http.Update(response)

	trans.ResponseTime = int32(msg.Ts.Sub(trans.ts).Nanoseconds() / 1e6) // resp_time in milliseconds

	// save Raw message
	if http.Send_response {
		trans.Response_raw = string(http.cutMessageBody(msg))
	}

	http.PublishTransaction(trans)

	logp.Debug("http", "HTTP transaction completed: %s\n", trans.Http)

	// remove from map
	delete(http.transactionsMap, trans.tuple.Hashable())
	if trans.timer != nil {
		trans.timer.Stop()
	}
}

func (http *Http) PublishTransaction(t *HttpTransaction) {

	if http.results == nil {
		return
	}

	event := common.MapStr{}

	event["type"] = "http"
	code := t.Http["code"].(uint16)
	if code < 400 {
		event["status"] = common.OK_STATUS
	} else {
		event["status"] = common.ERROR_STATUS
	}
	event["responsetime"] = t.ResponseTime
	if http.Send_request {
		event["request_raw"] = t.Request_raw
	}
	if http.Send_response {
		event["response_raw"] = t.Response_raw
	}
	event["http"] = t.Http
	if len(t.Real_ip) > 0 {
		event["real_ip"] = t.Real_ip
	}
	event["method"] = t.Method
	event["path"] = t.RequestUri

	event["@timestamp"] = common.Time(t.ts)
	event["src"] = &t.Src
	event["dst"] = &t.Dst

	http.results <- event
}

func splitCookiesHeader(headerVal string) map[string]string {
	cookies := map[string]string{}

	cstring := strings.Split(headerVal, ";")
	for _, cval := range cstring {
		cookie := strings.Split(cval, "=")
		cookies[strings.ToLower(strings.Trim(cookie[0], " "))] = cookie[1]
	}

	return cookies
}

func (http *Http) cutMessageBody(m *HttpMessage) []byte {
	raw_msg_cut := []byte{}

	// add headers always
	raw_msg_cut = m.Raw[:m.bodyOffset]

	// add body
	contentType, ok := m.Headers["content-type"]
	if ok && (len(contentType) == 0 || http.shouldIncludeInBody(contentType)) {
		if len(m.chunked_body) > 0 {
			raw_msg_cut = append(raw_msg_cut, m.chunked_body...)
		} else {
			logp.Debug("http", "Body to include: [%s]", m.Raw[m.bodyOffset:])
			raw_msg_cut = append(raw_msg_cut, m.Raw[m.bodyOffset:]...)
		}
	}

	return raw_msg_cut
}

func (http *Http) shouldIncludeInBody(contenttype string) bool {
	include_body := config.ConfigSingleton.Http.Include_body_for
	for _, include := range include_body {
		if strings.Contains(contenttype, include) {
			logp.Debug("http", "Should Include Body = true Content-Type "+contenttype+" include_body "+include)
			return true
		}
		logp.Debug("http", "Should Include Body = false Content-Type"+contenttype+" include_body "+include)
	}
	return false
}

func (http *Http) censorPasswords(m *HttpMessage, msg []byte) {

	keywords := config.ConfigSingleton.Passwords.Hide_keywords
	strip_authorization := config.ConfigSingleton.Passwords.Strip_authorization

	if m.IsRequest {
		// byte64 != encryption, so remove it from headers in case of Basic Authentication
		auth_text := []byte("Authorization:")
		if strip_authorization && (m.Headers["authorization"] != "") {
			header_len := m.bodyOffset - m.headerOffset
			val_start_x := bytes.Index(msg[m.headerOffset:m.bodyOffset], auth_text)
			val_end_x := -1
			if val_start_x != -1 {
				val_end_x = bytes.Index(msg[m.headerOffset+val_start_x:m.bodyOffset], []byte("\r\n"))

				if val_end_x < 0 || val_end_x > header_len {
					val_end_x = header_len
				}
				start_index := m.headerOffset + val_start_x + len(auth_text)
				end_index := m.headerOffset + val_end_x

				for i := start_index; i < end_index; i++ {
					msg[i] = byte('*')
				}
			}
			m.Headers["authorization"] = "*"
		}
		// passwords from POST forms in body
		if m.ContentLength > 0 && strings.Contains(m.Headers["content-type"], "urlencoded") {
			for _, keyword := range keywords {
				index := bytes.Index(msg[m.bodyOffset:], []byte(keyword))
				if index > 0 {
					start_index := m.bodyOffset + index + len(keyword)
					end_index := bytes.IndexAny(msg[m.bodyOffset+index+len(keyword):], "& \r\n")
					if end_index > 0 {
						end_index += m.bodyOffset + index
						if end_index > m.end {
							end_index = m.end
						}
					} else {
						end_index = m.end
					}

					if end_index-start_index < 120 {
						for i := start_index; i < end_index; i++ {
							msg[i] = byte('*')
						}
					}
				}
			}
		}
	}
}
