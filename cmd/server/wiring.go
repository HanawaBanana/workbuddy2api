package main

import (
	"workbuddy2api/internal/alert"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

// alertSource 把账号池与 WAF 门适配为 alert.Source。
//
// 为什么经适配器而非让 alert 直接依赖 pool/server：告警只需要"只读健康快照 +
// WAF 激活位"两个能力。窄接口让 internal/alert 不反向依赖网关内部结构
// （也就不受 pool.Handler 的字段改动牵连），测试里也能直接注入假数据源。
type alertSource struct {
	pool *pool.Pool
	h    *server.Handler
}

// Health 把 pool.RealmHealth 转成 alert.Health（只取告警需要的字段）。
func (s alertSource) Health(realm string) alert.Health {
	h := s.pool.RealmHealth(realm)
	return alert.Health{
		Total:    h.Total,
		Healthy:  h.Healthy,
		Cooling:  h.Cooling,
		Disabled: h.Disabled,
		Breaker:  h.Breaker,
		Degraded: h.Degraded,
		InFlight: h.InFlight,
	}
}

func (s alertSource) WAFActive() bool { return s.h.WAFActive() }
