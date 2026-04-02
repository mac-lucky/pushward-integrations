package metrics

import "context"

type ctxKey struct{}

// WithProvider returns a context carrying the provider name for metrics labelling.
func WithProvider(ctx context.Context, provider string) context.Context {
	return context.WithValue(ctx, ctxKey{}, provider)
}

// ProviderFromContext extracts the provider name from the context, or "unknown".
func ProviderFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return "unknown"
}
