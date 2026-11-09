package providers

import (
	"net/http"
	"testing"
)

func TestDirectEgressHasNoProxyFunctionAndProviderEgressKeepsOne(t *testing.T) {
	direct := NewDirectEgress()
	provider := NewEgress()
	defer direct.Client().CloseIdleConnections()
	defer provider.Client().CloseIdleConnections()
	transport := func(e *Egress) *http.Transport { return e.client.Transport.(*guarded).next.(*http.Transport) }
	if transport(direct).Proxy != nil || len(direct.proxies) != 0 {
		t.Fatal("direct egress retains a proxy lookup or proxy exemption")
	}
	if transport(provider).Proxy == nil {
		t.Fatal("provider environment proxy behavior changed")
	}
}
