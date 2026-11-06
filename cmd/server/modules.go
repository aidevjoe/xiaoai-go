package main

// 后台模块（插件）注册表。
//
// 内置页面在前端（ui/src/modules.js），这里只承载「后端才知道」的模块：
// 比如某个可选能力自带的诊断页、第三方插件提供的报表页。前端拿到
// /api/meta 的 modules 后会合并进导航，因此插件不需要改管理后台的代码。
//
// 约定（与前端 mergeRemoteModules 对齐）：
//
//	id      必填，全局唯一；和内置模块重名会被忽略（防止插件把内置页顶掉）
//	label   必填，侧栏显示名
//	group   run | assistant | voice | tools | system，未知值落到 system
//	icon    Ico 里的图标名，缺省 plug
//	url     插件页地址（同源）。提供 url 的模块以 iframe/链接形式打开，
//	        由插件自己负责构建与样式；不提供则必须由前端预注册组件。
//
// 目前没有可注册的模块，保留接口是为了让"加插件"变成一次 RegisterModule 调用，
// 而不是回头再改 meta 的返回结构和前端合并逻辑。
type adminModule struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
	Icon  string `json:"icon,omitempty"`
	Sub   string `json:"sub,omitempty"`
	URL   string `json:"url,omitempty"`
}

// RegisterModule 注册一个后台模块。幂等：同 id 覆盖旧定义（便于开发时热改）。
func (s *server) RegisterModule(m adminModule) {
	if m.ID == "" || m.Label == "" {
		return
	}
	s.modulesMu.Lock()
	defer s.modulesMu.Unlock()
	for i := range s.modules {
		if s.modules[i].ID == m.ID {
			s.modules[i] = m
			return
		}
	}
	s.modules = append(s.modules, m)
}

// moduleSnapshot 返回当前模块快照（复制一份，调用方随便读）。
// 用 make 而不是 append(nil,...)：空的时候要序列化成 []，前端拿到 null 会多一层判空。
func (s *server) moduleSnapshot() []adminModule {
	s.modulesMu.RLock()
	defer s.modulesMu.RUnlock()
	out := make([]adminModule, len(s.modules))
	copy(out, s.modules)
	return out
}
