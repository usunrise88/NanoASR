package httpx

import (
	"net"
	"net/http"
	"strings"
)

// LoopbackHosts are the Host headers a request to a loopback listener on
// port can honestly carry.
func LoopbackHosts(port string) []string {
	return []string{
		net.JoinHostPort("127.0.0.1", port),
		net.JoinHostPort("localhost", port),
		net.JoinHostPort("::1", port),
	}
}

// RequireHost refuses a request whose Host header is not one of hosts.
//
// A server on the loopback interface is reachable from any web page the user
// opens: the browser will not let the page read a response from another
// origin, but a name the page controls can be made to resolve to 127.0.0.1
// (DNS rebinding), and then the origin is the page's own. The Host header is
// what such a request cannot fake — it carries the attacker's name — so a
// listener that only ever expects to be called by its own address refuses
// every other one, before authentication and before the public pages.
func RequireHost(hosts ...string) Middleware {
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowed[strings.ToLower(r.Host)] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMisdirectedRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"this server answers only on its loopback address"}}` + "\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
