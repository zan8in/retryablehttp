package main

import (
	"fmt"

	"github.com/zan8in/retryablehttp"
)

func main() {
	resp, err := retryablehttp.DefaultClientPool.Get("http://example.com")
	fmt.Println(resp, err)
}
