// Package dataset holds the offline tools behind the universal provider's
// evaluation. Both are tests behind the fixtureexport build tag, so a plain
// go test ./... never runs them:
//
//	UNIVERSAL_EXPORT_OUT=fixtures.jsonl go test -tags fixtureexport -run TestExportFixtures ./internal/universal/dataset/
//	UNIVERSAL_PROPOSE_IN=in.jsonl UNIVERSAL_PROPOSE_OUT=out.jsonl go test -tags fixtureexport -run TestProposeJSONL ./internal/universal/dataset/
//
// The first drives every relay/testdata fixture through its provider's real
// handler and records the PushWard calls it makes; the second runs the
// heuristic over a JSONL of payloads, or of stored shapes, and writes each
// field's shape and path tokens with the proposal and the top candidates
// (UNIVERSAL_TOPK per role, 8 by default).
package dataset
