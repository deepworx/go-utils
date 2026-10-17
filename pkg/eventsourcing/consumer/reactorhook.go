package consumer

import "github.com/deepworx/go-utils/internal/consumerhook"

func init() {
	consumerhook.Register(consumerhook.Hooks{
		Backend:       runtimeBackend,
		ReactorOption: reactorOption,
		IsPermanent:   isPermanent,
	})
}

func runtimeBackend(rt any) (consumerhook.Backend, bool) {
	switch rt := rt.(type) {
	case *PgRuntime:
		if rt == nil {
			return consumerhook.Backend{}, false
		}
		return consumerhook.Backend{Codec: rt.codec, Pool: rt.pool, Schema: rt.schema}, true
	case *InMemoryRuntime:
		if rt == nil {
			return consumerhook.Backend{}, false
		}
		return consumerhook.Backend{Codec: rt.feed.Codec()}, true
	}
	return consumerhook.Backend{}, false
}

func reactorOption(ro consumerhook.ReactorOptions) any {
	return Option(func(o *options) {
		o.seedAtHead = ro.SeedAtHead
		o.decodeFailed = ro.DecodeFailed
		o.leased = ro.Leased
		o.drained = ro.Drained
	})
}
