//go:build !chromedp

package browser

import "context"

func (b *Browser) runWorkflow(ctx context.Context, workflow Workflow, options RunOptions, report *Report) error {
	report.Reason = "chrome_unavailable"
	return ErrChromeUnavailable
}
