// SPDX-License-Identifier: Apache-2.0

// Package mode is where the assembly layer turns a conversation's mode into an
// agent: which tools exist, what guards them, what the model is told, and what
// stands outside the loop as standing context.
//
// A mode is an assembly decision, so it lives here rather than in the engine:
// the engine builds the parts, and this package decides which of them a
// conversation is assembled from. There are two: coding, which carries a
// project's tools behind its gates, and chat, which carries none and is
// guarded by a gate that refuses every call, so that a conversation opened for
// words stays a conversation of words even if a later build wires a tool in
// by mistake.
package mode
