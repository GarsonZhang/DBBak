package api

import (
	"net/http"
)

func (s *Server) previewCleanup(w http.ResponseWriter, r *http.Request) {
	result, preview, err := s.cleanup.Preview(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "cleanup.preview", preview.JobID, "success")
	writeJSON(w, http.StatusCreated, map[string]any{"preview_id": preview.ID, "expires_at": preview.ExpiresAt, "result": result})
}

func (s *Server) executeCleanup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PreviewID string `json:"preview_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.PreviewID == "" {
		s.fail(w, r, http.StatusUnprocessableEntity, "PREVIEW_REQUIRED", "必须提供服务端清理预览 ID")
		return
	}
	executions, err := s.cleanup.Execute(r.Context(), input.PreviewID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	failed := 0
	for _, execution := range executions {
		if !execution.Deleted {
			failed++
		}
	}
	result := "success"
	if failed > 0 {
		result = "partial_failure"
	}
	s.audit(r, "cleanup.execute", input.PreviewID, result)
	writeJSON(w, http.StatusOK, map[string]any{"status": result, "executions": executions})
}
