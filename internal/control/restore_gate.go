package control

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/ronigooja/Zoe/internal/storage"
)

func (s *Server) restoreDeletionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if restoreDiagnosticRoute(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			writeError(w, apiError(http.StatusServiceUnavailable, "instance_state_unavailable", "instance state is unavailable", nil))
			return
		}
		err = storage.CheckRestoreDeletionReconciliationTx(r.Context(), tx)
		_ = tx.Rollback()
		if errors.Is(err, storage.ErrRestoreDeletionReconciliationRequired) {
			writeError(w, apiError(http.StatusServiceUnavailable, "restore_deletion_reconciliation_required", "restored instance is closed until deletion reconciliation is completed", nil))
			return
		}
		if err != nil {
			writeError(w, apiError(http.StatusServiceUnavailable, "instance_state_unavailable", "instance state is unavailable", nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func restoreDiagnosticRoute(method, path string) bool {
	return method == http.MethodGet && (path == "/v1/admin/instance" || path == "/v1/admin/instance/migration-readiness")
}
