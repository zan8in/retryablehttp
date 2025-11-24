package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	retryablehttp "github.com/zan8in/retryablehttp"
)

func main() {
	url := flag.String("url", "", "")
	method := flag.String("method", "GET", "")
	body := flag.String("body", "", "")
	proxy := flag.String("proxy", "", "")
	allowProxy := flag.Bool("allowproxy", false, "")
	limit := flag.Int64("limit", int64(2<<20), "")
	timeout := flag.Duration("timeout", 30*time.Second, "")
	useClient := flag.Bool("useclient", false, "")
	compare := flag.Bool("compare", false, "")
	flag.Parse()

	if *url == "" {
		fmt.Println("missing -url")
		os.Exit(2)
	}

	var bodyReader io.Reader
	if *body != "" {
		bodyReader = strings.NewReader(*body)
	}
	req, err := http.NewRequest(*method, *url, bodyReader)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	req.Header.Set("Accept", "text/html,application/xhtml xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	if *method == "POST" && *body != "" {
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}

	opts := retryablehttp.DefaultOptionsSingle
	opts.Timeout = *timeout
	opts.RawTCPMaxResponseBytes = *limit
	opts.RawTCPFallbackAllowProxy = *allowProxy
	if *proxy != "" {
		opts.Proxy = *proxy
	}

	client := retryablehttp.NewClient(opts)

	if *compare {
		var bodyData string
		if *body != "" {
			bodyData = *body
		}
		var br1 io.Reader
		var br2 io.Reader
		if bodyData != "" {
			br1 = strings.NewReader(bodyData)
			br2 = strings.NewReader(bodyData)
		}
		req1, _ := http.NewRequest(*method, *url, br1)
		req2, _ := http.NewRequest(*method, *url, br2)
		// copy headers
		for k, vals := range req.Header {
			for _, v := range vals {
				req1.Header.Add(k, v)
				req2.Header.Add(k, v)
			}
		}
		r1, e1 := client.RawTCPDo(req1)
		if e1 != nil {
			fmt.Println("rawtcp:", e1)
		} else {
			defer r1.Body.Close()
			b1, _ := io.ReadAll(r1.Body)
			fmt.Println("rawtcp:", r1.Status)
			fmt.Println(string(b1))
		}
		rreq, ewrap := retryablehttp.FromRequest(req2)
		if ewrap != nil {
			fmt.Println("wrap:", ewrap)
			os.Exit(1)
		}
		r2, e2 := client.Do(rreq)
		if e2 != nil {
			fmt.Println("do:", e2)
		} else {
			defer r2.Body.Close()
			b2, _ := io.ReadAll(r2.Body)
			fmt.Println("do:", r2.Status)
			fmt.Println(string(b2))
		}
		return
	}

	var resp *http.Response
	if *useClient {
		r, e := client.RawTCPDo(req)
		resp, err = r, e
	} else {
		r, e := retryablehttp.RawTCPDo(req, &opts)
		resp, err = r, e
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		fmt.Println(e)
		os.Exit(1)
	}
	fmt.Println(resp.Status)
	fmt.Println(string(b))
}
