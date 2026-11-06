//go:build !linux || !cgo || !sherpa

package audio

import "fmt"

// SherpaEngine 在非 linux / 未启用 cgo / 未加 sherpa 标签时是空实现，
// 保证 go build ./... 在任何平台都能通过，便于本机开发。
type SherpaEngine struct{}

// NewSherpaEngine 在本平台不可用。
func NewSherpaEngine(cfg SherpaConfig) (Engine, error) {
	return nil, fmt.Errorf("%w: 当前构建未包含 sherpa 引擎（需 linux + cgo + -tags sherpa）", ErrNotImplemented)
}
