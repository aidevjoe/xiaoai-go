//go:build !tls

package wsclient

import (
	"context"
	"fmt"
	"net"
	"time"
)

// rawConnect 建 TCP 连接（默认构建：仅支持 ws://）。
//
// 不编译 TLS 是有意为之：音箱只在局域网连自己的服务端，
// 带上 crypto/tls + crypto/x509 会让二进制白白多 2MB 左右。
// 真要走 wss 时用 `go build -tags tls`。
func rawConnect(ctx context.Context, u target, timeout time.Duration) (net.Conn, error) {
	if u.secure {
		return nil, fmt.Errorf("wsclient: 本版本不支持 wss://（需要 go build -tags tls）")
	}
	return dialTCP(ctx, u, timeout)
}
