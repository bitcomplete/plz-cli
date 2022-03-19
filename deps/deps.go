package deps

import (
	"context"
	"log"

	"github.com/bitcomplete/plz-cli/client/auth"
)

type depsKeyType int

var depsKey depsKeyType

type Deps struct {
	ErrorLog *log.Logger
	InfoLog  *log.Logger
	DebugLog *log.Logger
	*auth.Auth
	PlzAPIBaseURL string
}

func ContextWithDeps(ctx context.Context, deps *Deps) context.Context {
	return context.WithValue(ctx, depsKey, deps)
}

func FromContext(ctx context.Context) *Deps {
	deps := ctx.Value(depsKey).(*Deps)
	if deps == nil {
		return &Deps{}
	}
	return deps
}
