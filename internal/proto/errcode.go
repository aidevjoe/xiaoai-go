package proto

// 错误码。0 表示成功；负数保留给协议层，正数留给业务层。
const (
	CodeOK           = 0
	CodeUnknown      = -1
	CodeBadRequest   = -2 // 负载解析失败 / 参数非法
	CodeUnknownCmd   = -3 // 未知命令
	CodeTimeout      = -4
	CodeUnavailable  = -5 // 设备忙 / 资源不可用
	CodeUnauthorized = -6
	CodeUnsupported  = -7 // 设备不支持该能力
	CodeInternal     = -8
)

// CodeText 返回错误码的中文描述，用于日志与后台展示。
func CodeText(code int) string {
	switch code {
	case CodeOK:
		return "ok"
	case CodeBadRequest:
		return "参数错误"
	case CodeUnknownCmd:
		return "未知命令"
	case CodeTimeout:
		return "执行超时"
	case CodeUnavailable:
		return "设备忙或资源不可用"
	case CodeUnauthorized:
		return "未授权"
	case CodeUnsupported:
		return "设备不支持"
	case CodeInternal:
		return "服务端内部错误"
	default:
		return "未知错误"
	}
}
