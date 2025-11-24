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

	opts := retryablehttp.DefaultOptionsSingle
	opts.Timeout = *timeout
	opts.RawTCPMaxResponseBytes = *limit
	opts.RawTCPFallbackAllowProxy = *allowProxy
	if *proxy != "" {
		opts.Proxy = *proxy
	}

	client := retryablehttp.NewClient(opts)

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
