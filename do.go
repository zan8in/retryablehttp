package retryablehttp

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dac "github.com/Mzack9999/go-http-digest-auth-client"
	xproxy "golang.org/x/net/proxy"
)

// PassthroughErrorHandler is an ErrorHandler that directly passes through the
// values from the net/http library for the final request. The body is not
// closed.
func PassthroughErrorHandler(resp *http.Response, err error, _ int) (*http.Response, error) {
	return resp, err
}

// Do wraps calling an HTTP method with retries.
func (c *Client) Do(req *Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	// Create a main context that will be used as the main timeout
	mainCtx, cancel := context.WithTimeout(context.Background(), c.options.Timeout)
	defer cancel()

	retryMax := c.options.RetryMax
	if ctxRetryMax := req.Context().Value(RETRY_MAX); ctxRetryMax != nil {
		if maxRetriesParsed, ok := ctxRetryMax.(int); ok {
			retryMax = maxRetriesParsed
		}
	}

	didRawTCP := false
	for i := 0; ; i++ {
		// request body can be read multiple times
		// hence no need to rewind it

		if c.RequestLogHook != nil {
			c.RequestLogHook(req.Request, i)
		}

		if req.hasAuth() && req.Auth.Type == DigestAuth {
			digestTransport := dac.NewTransport(req.Auth.Username, req.Auth.Password)
			digestTransport.HTTPClient = c.HTTPClient
			resp, err = digestTransport.RoundTrip(req.Request)
		} else {
			// Attempt the request with standard behavior
			resp, err = c.HTTPClient.Do(req.Request)
		}

		if !didRawTCP && c.options.RawTCPFallbackEnabled {
			mallowed := len(c.options.RawTCPFallbackMethods) == 0
			if !mallowed {
				for _, m := range c.options.RawTCPFallbackMethods {
					if req.Request.Method == m {
						mallowed = true
						break
					}
				}
			}
			pblocked := (ProxyURL != "" || ProxySocksURL != "" || c.options.Proxy != "") && !c.options.RawTCPFallbackAllowProxy
			patterns := c.options.RawTCPFallbackErrorPatterns
			if len(patterns) == 0 {
				patterns = retryableErrors
			}
			if mallowed && !pblocked {
				emsg := ""
				if err != nil {
					emsg = err.Error()
				}
				match := false
				for _, kw := range patterns {
					if kw != "" && strings.Contains(emsg, kw) {
						match = true
						break
					}
				}
				if match {
					rawResp, rawErr := c.rawTCPRequest(req.Request)
					if rawErr == nil {
						resp = rawResp
						err = nil
						didRawTCP = true
					}
				}
			}
		}

		// Check if we should continue with retries.
		checkOK, checkErr := c.CheckRetry(req.Context(), resp, err)

		// if err is equal to missing minor protocol version retry with http/2
		if err != nil && strings.Contains(err.Error(), "net/http: HTTP/1.x transport connection broken: malformed HTTP version \"HTTP/2\"") {
			resp, err = c.HTTPClient2.Do(req.Request)
			checkOK, checkErr = c.CheckRetry(req.Context(), resp, err)
		}

		if err != nil {
			// Increment the failure counter as the request failed
			req.Metrics.Failures++
		} else {
			// Call this here to maintain the behavior of logging all requests,
			// even if CheckRetry signals to stop.
			if c.ResponseLogHook != nil {
				// Call the response logger function if provided.
				c.ResponseLogHook(resp)
			}
		}

		// Now decide if we should continue.
		if !checkOK {
			if checkErr != nil {
				err = checkErr
			}
			c.closeIdleConnections()
			return resp, err
		}

		// We do this before drainBody beause there's no need for the I/O if
		// we're breaking out
		remain := retryMax - i
		if remain <= 0 {
			break
		}

		// Increment the retries counter as we are going to do one more retry
		req.Metrics.Retries++

		// We're going to retry, consume any response to reuse the connection.
		if err == nil && resp != nil {
			c.drainBody(req, resp)
		}

		// Wait for the time specified by backoff then retry.
		// If the context is cancelled however, return.
		wait := c.Backoff(c.options.RetryWaitMin, c.options.RetryWaitMax, i, resp)

		// Exit if the main context or the request context is done
		// Otherwise, wait for the duration and try again.
		// use label to explicitly specify what to break
	selectstatement:
		select {
		case <-mainCtx.Done():
			break selectstatement
		case <-req.Context().Done():
			c.closeIdleConnections()
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}

	if c.ErrorHandler != nil {
		c.closeIdleConnections()
		return c.ErrorHandler(resp, err, retryMax+1)
	}

	// By default, we close the response body and return an error without
	// returning the response
	if resp != nil {
		resp.Body.Close()
	}
	c.closeIdleConnections()
	return nil, fmt.Errorf("%s %s giving up after %d attempts: %w", req.Method, req.URL, retryMax+1, err)
}

// Try to read the response body so we can reuse this connection.
func (c *Client) drainBody(req *Request, resp *http.Response) {
	_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, c.options.RespReadLimit))
	if err != nil {
		req.Metrics.DrainErrors++
	}
	resp.Body.Close()
}

const closeConnectionsCounter = 100
const rawTCPMaxResponseBytes = 2 * 1024 * 1024

func (c *Client) closeIdleConnections() {
	if c.options.KillIdleConn {
		requestCounter := atomic.LoadUint32(&c.requestCounter)
		if requestCounter < closeConnectionsCounter {
			atomic.AddUint32(&c.requestCounter, 1)
		} else {
			atomic.StoreUint32(&c.requestCounter, 0)
			c.HTTPClient.CloseIdleConnections()
			c.HTTPClient2.CloseIdleConnections()
		}
	}
}

// 定义需要触发备用请求的错误关键词
var retryableErrors = []string{
	"malformed HTTP response",
	"malformed HTTP version",
	"unexpected EOF",
}

// 检查错误是否匹配
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	for _, keyword := range retryableErrors {
		if strings.Contains(errMsg, keyword) {
			return true
		}
	}
	return false
}

// 实现原始 TCP 请求
func (c *Client) rawTCPRequest(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error
	if len(c.options.Proxy) > 0 && len(ProxyURL) == 0 && len(ProxySocksURL) == 0 {
		timeoutSec := int(c.options.Timeout / time.Second)
		if timeoutSec <= 0 {
			timeoutSec = DefaultTimeout
		}
		_ = LoadProxyServersWithTimeout(c.options.Proxy, timeoutSec)
	}
	useProxy := c.options.RawTCPFallbackAllowProxy && (ProxyURL != "" || ProxySocksURL != "" || c.options.Proxy != "")
	if useProxy && ProxySocksURL != "" {
		su, sErr := url.Parse(ProxySocksURL)
		if sErr != nil {
			return nil, sErr
		}
		d, dErr := xproxy.FromURL(su, &net.Dialer{Timeout: c.options.Timeout})
		if dErr != nil {
			return nil, dErr
		}
		conn, err = d.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		if req.URL.Scheme == "https" {
			tc := &tls.Config{InsecureSkipVerify: true}
			conn = tls.Client(conn, tc)
		}
	} else if useProxy && ProxyURL != "" {
		pu, pErr := url.Parse(ProxyURL)
		if pErr != nil {
			return nil, pErr
		}
		paddr := net.JoinHostPort(pu.Hostname(), func() string {
			if pu.Port() != "" {
				return pu.Port()
			} else {
				if pu.Scheme == "https" {
					return "443"
				}
				return "80"
			}
		}())
		var pconn net.Conn
		if pu.Scheme == "https" {
			d := &net.Dialer{Timeout: c.options.Timeout}
			pconn, err = tls.DialWithDialer(d, "tcp", paddr, &tls.Config{InsecureSkipVerify: true})
		} else {
			if c.options.Timeout > 0 {
				pconn, err = net.DialTimeout("tcp", paddr, c.options.Timeout)
			} else {
				pconn, err = net.Dial("tcp", paddr)
			}
		}
		if err != nil {
			return nil, err
		}
		var authHeader string
		if pu.User != nil {
			u := pu.User.Username()
			pw, _ := pu.User.Password()
			token := base64.StdEncoding.EncodeToString([]byte(u + ":" + pw))
			authHeader = "Proxy-Authorization: Basic " + token + "\r\n"
		}
		if req.URL.Scheme == "https" {
			cl := "CONNECT " + host + ":" + port + " HTTP/1.0\r\n" + "Host: " + host + ":" + port + "\r\n" + authHeader + "\r\n"
			if c.options.Timeout > 0 {
				_ = pconn.SetDeadline(time.Now().Add(c.options.Timeout))
			}
			if _, err := pconn.Write([]byte(cl)); err != nil {
				pconn.Close()
				return nil, err
			}
			br := bufio.NewReader(pconn)
			var headerBuf bytes.Buffer
			for {
				line, rErr := br.ReadString('\n')
				if rErr != nil {
					pconn.Close()
					return nil, rErr
				}
				headerBuf.WriteString(line)
				if strings.TrimSpace(line) == "" {
					break
				}
			}
			first := headerBuf.String()
			if !strings.Contains(first, " 200 ") {
				pconn.Close()
				return nil, fmt.Errorf("proxy CONNECT failed")
			}
			tc := &tls.Config{InsecureSkipVerify: true}
			conn = tls.Client(pconn, tc)
			if c.options.Timeout > 0 {
				_ = conn.SetDeadline(time.Now().Add(c.options.Timeout))
			}
		} else {
			conn = pconn
			reqURLAbs := req.URL.String()
			requestLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", req.Method, reqURLAbs)
			headers := "Host: " + req.URL.Host + "\r\n" + authHeader
			headers += "Connection: close\r\n"
			headers += "Proxy-Connection: close\r\n"
			for k, vals := range req.Header {
				if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Proxy-Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
					continue
				}
				for _, v := range vals {
					headers += k + ": " + v + "\r\n"
				}
			}
			var bodyBytes []byte
			teChunked := false
			if req.Body != nil {
				bodyBytes, _ = io.ReadAll(req.Body)
				if req.ContentLength > 0 {
					headers += fmt.Sprintf("Content-Length: %d\r\n", int(req.ContentLength))
				} else {
					headers += "Transfer-Encoding: chunked\r\n"
					teChunked = true
				}
			}
			request := requestLine + headers + "\r\n"
			if len(bodyBytes) > 0 {
				if teChunked {
					request += fmt.Sprintf("%x\r\n", len(bodyBytes)) + string(bodyBytes) + "\r\n0\r\n\r\n"
				} else {
					request += string(bodyBytes)
				}
			}
			if c.options.Timeout > 0 {
				_ = conn.SetDeadline(time.Now().Add(c.options.Timeout))
			}
			if _, err := conn.Write([]byte(request)); err != nil {
				conn.Close()
				return nil, err
			}
			buf := new(bytes.Buffer)
			limit := c.options.RawTCPMaxResponseBytes
			if limit <= 0 {
				limit = rawTCPMaxResponseBytes
			}
			lr := io.LimitReader(conn, limit)
			if _, err := buf.ReadFrom(lr); err != nil {
				conn.Close()
				return nil, err
			}
			rawResponse := buf.Bytes()
			if len(rawResponse) == 0 {
				conn.Close()
				return nil, fmt.Errorf("empty raw response")
			}
			br := bufio.NewReader(bytes.NewReader(rawResponse))
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				headerEnd := bytes.Index(rawResponse, []byte("\r\n\r\n"))
				var headerBytes []byte
				var body []byte
				if headerEnd >= 0 && len(rawResponse) >= headerEnd+4 {
					headerBytes = rawResponse[:headerEnd]
					body = rawResponse[headerEnd+4:]
				} else {
					body = rawResponse
				}
				header := make(http.Header)
				statusCode := 200
				proto := "HTTP/1.0"
				if len(headerBytes) > 0 {
					firstLineEnd := bytes.IndexByte(headerBytes, '\n')
					if firstLineEnd != -1 {
						firstLine := string(bytes.TrimSpace(headerBytes[:firstLineEnd]))
						parts := strings.SplitN(firstLine, " ", 3)
						if len(parts) >= 2 {
							proto = parts[0]
							statusCode, _ = strconv.Atoi(parts[1])
						}
					}
					rawHeaders := bytes.Split(headerBytes, []byte("\r\n"))
					for _, line := range rawHeaders[1:] {
						if colonIndex := bytes.IndexByte(line, ':'); colonIndex != -1 {
							key := string(bytes.TrimSpace(line[:colonIndex]))
							value := string(bytes.TrimSpace(line[colonIndex+1:]))
							header.Add(key, value)
						}
					}
				}
				enc := strings.ToLower(header.Get("Content-Encoding"))
				if enc != "" {
					if db, ok := decodeBody(enc, body); ok {
						body = db
						header.Del("Content-Encoding")
					}
				}
				if header.Get("Content-Length") == "" {
					header.Set("Content-Length", strconv.Itoa(len(body)))
				}
				if header.Get("Content-Type") == "" {
					header.Set("Content-Type", "application/octet-stream")
				}
				return &http.Response{Status: fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)), StatusCode: statusCode, Proto: proto, ProtoMajor: 1, ProtoMinor: 0, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req, ContentLength: int64(len(body))}, nil
			}
			b, e := io.ReadAll(resp.Body)
			if e == nil {
				enc := strings.ToLower(resp.Header.Get("Content-Encoding"))
				if enc != "" {
					if db, ok := decodeBody(enc, b); ok {
						b = db
						resp.Header.Del("Content-Encoding")
					}
				}
				resp.Body = io.NopCloser(bytes.NewReader(b))
				resp.ContentLength = int64(len(b))
				resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			}
			return resp, nil
		}
	} else {
		if req.URL.Scheme == "https" {
			d := &net.Dialer{Timeout: c.options.Timeout}
			conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
		} else {
			if c.options.Timeout > 0 {
				conn, err = net.DialTimeout("tcp", addr, c.options.Timeout)
			} else {
				conn, err = net.Dial("tcp", addr)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if c.options.Timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(c.options.Timeout))
	}
	defer conn.Close()
	requestLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", req.Method, req.URL.RequestURI())
	headers := "Host: " + req.URL.Host + "\r\n"
	headers += "Connection: close\r\n"
	for k, vals := range req.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vals {
			headers += k + ": " + v + "\r\n"
		}
	}
	var bodyBytes []byte
	teChunked := false
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		if req.ContentLength > 0 {
			headers += fmt.Sprintf("Content-Length: %d\r\n", int(req.ContentLength))
		} else {
			headers += "Transfer-Encoding: chunked\r\n"
			teChunked = true
		}
	}
	request := requestLine + headers + "\r\n"
	if len(bodyBytes) > 0 {
		if teChunked {
			request += fmt.Sprintf("%x\r\n", len(bodyBytes)) + string(bodyBytes) + "\r\n0\r\n\r\n"
		} else {
			request += string(bodyBytes)
		}
	}
	if _, err := conn.Write([]byte(request)); err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	limit := c.options.RawTCPMaxResponseBytes
	if limit <= 0 {
		limit = rawTCPMaxResponseBytes
	}
	lr := io.LimitReader(conn, limit)
	if _, err := buf.ReadFrom(lr); err != nil {
		return nil, err
	}

	rawResponse := buf.Bytes()
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("empty raw response")
	}
	br := bufio.NewReader(bytes.NewReader(rawResponse))
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		headerEnd := bytes.Index(rawResponse, []byte("\r\n\r\n"))
		var headerBytes []byte
		var body []byte
		if headerEnd >= 0 && len(rawResponse) >= headerEnd+4 {
			headerBytes = rawResponse[:headerEnd]
			body = rawResponse[headerEnd+4:]
		} else {
			body = rawResponse
		}
		header := make(http.Header)
		statusCode := 200
		proto := "HTTP/1.0"
		if len(headerBytes) > 0 {
			firstLineEnd := bytes.IndexByte(headerBytes, '\n')
			if firstLineEnd != -1 {
				firstLine := string(bytes.TrimSpace(headerBytes[:firstLineEnd]))
				parts := strings.SplitN(firstLine, " ", 3)
				if len(parts) >= 2 {
					proto = parts[0]
					statusCode, _ = strconv.Atoi(parts[1])
				}
			}
			rawHeaders := bytes.Split(headerBytes, []byte("\r\n"))
			for _, line := range rawHeaders[1:] {
				if colonIndex := bytes.IndexByte(line, ':'); colonIndex != -1 {
					key := string(bytes.TrimSpace(line[:colonIndex]))
					value := string(bytes.TrimSpace(line[colonIndex+1:]))
					header.Add(key, value)
				}
			}
		}
		enc := strings.ToLower(header.Get("Content-Encoding"))
		if enc != "" {
			if db, ok := decodeBody(enc, body); ok {
				body = db
				header.Del("Content-Encoding")
			}
		}
		if header.Get("Content-Length") == "" {
			header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/octet-stream")
		}
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
			StatusCode:    statusCode,
			Proto:         proto,
			ProtoMajor:    1,
			ProtoMinor:    0,
			Header:        header,
			Body:          io.NopCloser(bytes.NewReader(body)),
			Request:       req,
			ContentLength: int64(len(body)),
		}, nil
	}
	b, e := io.ReadAll(resp.Body)
	if e == nil {
		enc := strings.ToLower(resp.Header.Get("Content-Encoding"))
		if enc != "" {
			if db, ok := decodeBody(enc, b); ok {
				b = db
				resp.Header.Del("Content-Encoding")
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}
	return resp, nil
}

func (c *Client) RawTCPDo(req *http.Request) (*http.Response, error) {
	return c.rawTCPRequest(req)
}

func RawTCPDo(req *http.Request, opts *Options) (*http.Response, error) {
	var options Options
	if opts != nil {
		options = *opts
	} else {
		options = DefaultOptionsSingle
	}
	c := &Client{options: options}
	return c.rawTCPRequest(req)
}
func decodeBody(enc string, data []byte) ([]byte, bool) {
	switch strings.ToLower(enc) {
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false
		}
		defer gr.Close()
		b, err := io.ReadAll(gr)
		if err != nil {
			return nil, false
		}
		return b, true
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err == nil {
			b, e := io.ReadAll(zr)
			zr.Close()
			if e == nil {
				return b, true
			}
		}
		fr := flate.NewReader(bytes.NewReader(data))
		defer fr.Close()
		b, e := io.ReadAll(fr)
		if e == nil {
			return b, true
		}
		return nil, false
	default:
		return nil, false
	}
}
