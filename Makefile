.DEFAULT_GOAL := specs

.PHONY: specs

# specs checks the spec tree against the gate. The full gate (make check)
# arrives with the scaffold, specs/016-distribution.md.
specs:
	go tool lateregate spec-lint
