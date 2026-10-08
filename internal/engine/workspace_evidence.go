package engine

func (e *Engine) InvalidateWorkspaceEvidence() {
	e.invalidateSavedAcceptance()
}
