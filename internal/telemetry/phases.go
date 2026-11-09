// Package telemetry produces bounded request-phase traces without retaining request content or caller identity.
package telemetry

// Phase fixes trace names at compile time; request values cannot introduce attributes or span names.
type Phase uint8

const (
	GatewayRequest Phase = iota + 1
	GatewayPrepare
	GatewayAdmission
	GatewayDispatch
	GatewayRelay
	GatewaySettlement
	GatewayFirstData
	GatewayFirstOutput
	GatewayFirstFlush
	NodeRequest
	NodePermit
	NodeEngineRequest
	NodeEngineHeaders
	NodeEngineRelay
)

var phaseNames = [...]string{"", "gateway.request", "gateway.prepare", "gateway.admission", "gateway.dispatch",
	"gateway.relay", "gateway.settlement", "gateway.first_data", "gateway.first_output", "gateway.first_flush",
	"node.request", "node.permit", "node.engine_request", "node.engine_headers", "node.engine_relay"}

func (p Phase) valid() bool { return p > 0 && int(p) < len(phaseNames) }

func (p Phase) kind() int {
	switch p {
	case GatewayRequest, NodeRequest:
		return 2
	case GatewayDispatch, NodeEngineRequest:
		return 3
	default:
		return 1
	}
}

// Result uses OTLP's fixed unset, OK and error codes, without error descriptions.
type Result uint8

const (
	Unset Result = iota
	OK
	Error
)
