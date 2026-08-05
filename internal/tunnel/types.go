package tunnel

const (
	RoleAgent        = "agent"
	RoleClient       = "client"
	RoleAgentControl = "agent-control"
	RoleAgentSession = "agent-session"
)

type LogFunc func(format string, args ...any)

func NoopLog(string, ...any) {}
