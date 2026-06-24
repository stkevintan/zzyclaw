package agent

import "testing"

func TestCanManageShared(t *testing.T) {
	owners := map[string]struct{}{"alice": {}}

	if !canManageShared(owners, "alice") {
		t.Error("listed owner should be allowed to manage shared skills")
	}
	if canManageShared(owners, "bob") {
		t.Error("non-owner must not manage shared skills when owners are configured")
	}

	// With no owners configured the gate is disabled and everyone is allowed,
	// matching the engine's dangerous-tool owner gate.
	if !canManageShared(map[string]struct{}{}, "bob") {
		t.Error("empty owners should allow everyone")
	}
}
