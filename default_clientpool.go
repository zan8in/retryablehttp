package retryablehttp

import "fmt"

var (
	DefaultClientPool  *Client
	RedirectClientPool *Client
)

var options *PoolOptions

func init() {
	options = &DefaultPoolOptions
	InitClientPool(options)
	fmt.Println(options.Threads)

	DefaultClientPool, _ = GetPool(options)

	options.EnableRedirect(FollowAllRedirect)
	RedirectClientPool, _ = GetPool(options)
}
