//go:build tls

package wsclient

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// rawConnect 建 TCP/TLS 连接（`-tags tls` 构建：支持 wss://）。
//
// 这个 tag 会引入 crypto/tls 与 crypto/x509，二进制大约多 2MB，
// 只有把服务端暴露到公网、必须走 TLS 时才需要。
func rawConnect(ctx context.Context, u target, timeout time.Duration) (net.Conn, error) {
	nc, err := dialTCP(ctx, u, timeout)
	if err != nil {
		return nil, err
	}
	if !u.secure {
		return nc, nil
	}
	// ServerName 用 host 去端口
	host := u.host
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			host = host[:i]
			break
		}
	}
	tc := tls.Client(nc, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return tc, nil
}
