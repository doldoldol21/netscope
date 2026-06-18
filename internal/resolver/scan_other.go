//go:build !darwin

package resolver

import "github.com/doldoldol21/netscope/pkg/types"

// scan is a stub on non-macOS platforms. netscope's Phase 1 target is macOS;
// this keeps the package compilable elsewhere (CI, editors) while making the
// lack of attribution explicit.
func scan(pathCache map[int]types.Process) ([]rawConn, map[int]types.Process) {
	return nil, pathCache
}
