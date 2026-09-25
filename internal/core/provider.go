// SPDX-License-Identifier: Apache-2.0

package core

import "context"

// Provider streams model output for one chat request.
type Provider interface {
	ChatStream(ctx context.Context, request ChatRequest) (<-chan StreamChunk, error)
	Name() string
}
