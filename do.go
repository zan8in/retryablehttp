package retryablehttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dac "github.com/Mzack9999/go-http-digest-auth-client"
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

		// 在 Do 方法中添加备用请求逻辑
		if isRetryableError(err) {
			rawResp, rawErr := c.rawTCPRequest(req.Request)
			if rawErr == nil {
				resp = rawResp
				err = nil
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

func (c *Client) closeIdleConnections() {
	if c.options.KillIdleConn {
		requestCounter := atomic.LoadUint32(&c.requestCounter)
		if requestCounter < closeConnectionsCounter {
			atomic.AddUint32(&c.requestCounter, 1)
		} else {
			atomic.StoreUint32(&c.requestCounter, 0)
			c.HTTPClient.CloseIdleConnections()
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
	conn, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 构造原始 HTTP 请求
	requestLine := fmt.Sprintf("%s %s HTTP/1.0\r\n", req.Method, req.URL.RequestURI())
	headers := "Host: " + req.URL.Host + "\r\n"
	headers += "Connection: close\r\n"
	if req.Body != nil {
		bodyBytes, _ := io.ReadAll(req.Body)
		headers += fmt.Sprintf("Content-Length: %d\r\n", len(bodyBytes))
		requestLine += headers + "\r\n" + string(bodyBytes)
	} else {
		requestLine += headers + "\r\n"
	}

	// 发送请求
	if _, err := conn.Write([]byte(requestLine)); err != nil {
		return nil, err
	}

	// 读取原始响应数据
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(conn); err != nil {
		return nil, err
	}

	// 1. 解析原始响应数据中的 Header 部分
	rawResponse := buf.Bytes()
	headerEnd := bytes.Index(rawResponse, []byte("\r\n\r\n")) // 查找头与体的分界
	if headerEnd == -1 {
		// 若没有完整头，默认视为无头响应
		headerEnd = 0
	}

	// 2. 分割 Header 和 Body
	headerBytes := rawResponse[:headerEnd]
	bodyBytes := rawResponse[headerEnd+4:] // 跳过两个 CRLF

	// 3. 动态解析 Header
	header := make(http.Header)
	statusCode := 200 // 默认状态码
	proto := "HTTP/1.0"

	if len(headerBytes) > 0 {
		// 解析状态行（如 "HTTP/1.0 200 OK"）
		firstLineEnd := bytes.IndexByte(headerBytes, '\n')
		if firstLineEnd != -1 {
			firstLine := string(bytes.TrimSpace(headerBytes[:firstLineEnd]))
			parts := strings.SplitN(firstLine, " ", 3)
			if len(parts) >= 2 {
				proto = parts[0]                       // 协议版本（如 HTTP/1.0）
				statusCode, _ = strconv.Atoi(parts[1]) // 状态码
			}
		}

		// 解析头字段（如 "Content-Type: application/octet-stream"）
		rawHeaders := bytes.Split(headerBytes, []byte("\r\n"))
		for _, line := range rawHeaders[1:] { // 跳过状态行
			if colonIndex := bytes.IndexByte(line, ':'); colonIndex != -1 {
				key := string(bytes.TrimSpace(line[:colonIndex]))
				value := string(bytes.TrimSpace(line[colonIndex+1:]))
				header.Add(key, value)
			}
		}
	}

	// 4. 补充必要 Header（若缺失）
	if header.Get("Content-Length") == "" {
		header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/octet-stream") // 默认值
	}

	// 5. 构造 Response 对象
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		StatusCode:    statusCode,
		Proto:         proto,
		ProtoMajor:    1, // 假设为 HTTP/1.x
		ProtoMinor:    0,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(bodyBytes)),
		Request:       req,
		ContentLength: int64(len(bodyBytes)),
	}, nil
}
