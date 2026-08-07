package proxy

import "net/http"

// UtilityRoutes are the upstream's own routes, outside the OpenAI surface and
// so mounted at the root rather than under /v1. Each one is a lookup: it reads
// what the server already has loaded, or runs the tokenizer. None of them
// generate, so none of them produce tokens to bill.
//
// Generating routes are absent because their replies report token counts in the
// upstream's own shape instead of an OpenAI usage block, which nothing here can
// meter. Routes carrying server state are absent deliberately: /slots hands
// back the prompt sitting in each slot, and POST to /props or /lora-adapters
// changes the server underneath every other caller.
//
// The router mounts exactly this list, so what is forwarded and what can be
// metered cannot drift apart.
// /models is the upstream's own listing and not the /v1/models being forwarded
// under the OpenAI prefix. The two answer differently, so one cannot stand in
// for the other, and the root one has to reach the root.
var UtilityRoutes = []struct{ Method, Path string }{
	{http.MethodGet, "/health"},
	{http.MethodGet, "/props"},
	{http.MethodGet, "/models"},
	{http.MethodPost, "/tokenize"},
	{http.MethodPost, "/detokenize"},
	{http.MethodPost, "/apply-template"},
}

var utilityPaths = func() map[string]bool {
	m := make(map[string]bool, len(UtilityRoutes))
	for _, rt := range UtilityRoutes {
		m[rt.Path] = true
	}
	return m
}()

// isUtilityPath reports whether a path is one of the forwarded utility routes.
// The method is already settled by the mount, so the path alone answers it.
func isUtilityPath(p string) bool { return utilityPaths[p] }
