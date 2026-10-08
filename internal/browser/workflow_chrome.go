//go:build chromedp

package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

func workflowChromePath(explicit string) string {
	if explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return explicit
		}
		return ""
	}
	for _, candidate := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge",
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

func (b *Browser) runWorkflow(ctx context.Context, workflow Workflow, options RunOptions, report *Report) error {
	path := workflowChromePath(options.ChromePath)
	if path == "" {
		report.Reason = "chrome_unavailable"
		return ErrChromeUnavailable
	}
	proxyCtx, cancelProxy := context.WithCancel(ctx)
	defer cancelProxy()
	proxy, err := b.startWorkflowProxy(proxyCtx)
	if err != nil {
		report.Reason = "proxy_unavailable"
		return errors.New("browser proxy unavailable")
	}
	defer proxy.close()
	for _, viewport := range []struct {
		name          string
		width, height int64
		mobile        bool
	}{{"desktop", 1280, 800, false}, {"mobile", 390, 844, true}} {
		err = b.runWorkflowViewport(ctx, workflow, path, proxy, report, viewport.name, viewport.width, viewport.height, viewport.mobile)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				report.Reason = "cancelled_or_timeout"
				return ctx.Err()
			case proxy.blocked.Load() || errors.Is(err, errWorkflowSafety):
				report.Status, report.Reason = StatusFail, "safety_policy"
				return errWorkflowSafety
			case errors.Is(err, ErrAssertionFailed):
				report.Status, report.Reason = StatusFail, "assertion_failed"
				return ErrAssertionFailed
			case proxy.failed.Load() || errors.Is(err, errWorkflowNetwork):
				report.Reason = "network_unavailable"
				return errWorkflowNetwork
			default:
				report.Reason = "browser_execution_failed"
				return errors.New("browser workflow execution failed")
			}
		}
	}
	if proxy.blocked.Load() {
		report.Status, report.Reason = StatusFail, "safety_policy"
		return errWorkflowSafety
	}
	if proxy.failed.Load() {
		report.Reason = "network_unavailable"
		return errWorkflowNetwork
	}
	report.Status = StatusPass
	return nil
}

func (b *Browser) runWorkflowViewport(ctx context.Context, workflow Workflow, chromePath string, proxy *workflowProxy, report *Report, viewport string, width, height int64, mobile bool) error {
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.ExecPath(chromePath), chromedp.ProxyServer("http://"+proxy.address),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"), chromedp.Flag("disable-quic", true),
		chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"),
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("disable-features", "WebTransport"))
	allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, options...)
	defer cancelAllocator()
	tab, cancelTab := chromedp.NewContext(allocator, chromedp.WithErrorf(func(string, ...any) {}))
	defer cancelTab()
	chromedp.ListenTarget(tab, func(event any) {
		paused, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go func() {
			blocked := paused.Request == nil
			if paused.Request != nil {
				raw := paused.Request.URL
				blocked = b.workflowURL(tab, raw) != nil
				if strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "blob:") || raw == "about:blank" {
					blocked = false
				}
			}
			if blocked {
				proxy.blocked.Store(true)
			}
			chrome := chromedp.FromContext(tab)
			if chrome == nil || chrome.Target == nil {
				return
			}
			executor := cdp.WithExecutor(tab, chrome.Target)
			if blocked {
				_ = fetch.FailRequest(paused.RequestID, network.ErrorReasonBlockedByClient).Do(executor)
			} else {
				_ = fetch.ContinueRequest(paused.RequestID).Do(executor)
			}
		}()
	})
	viewportOptions := []chromedp.EmulateViewportOption{}
	if mobile {
		viewportOptions = append(viewportOptions, chromedp.EmulateMobile)
	}
	if err := chromedp.Run(tab, fetch.Enable(), browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny),
		chromedp.EmulateViewport(width, height, viewportOptions...), chromedp.Navigate(workflow.URL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	if proxy.failed.Load() {
		return errWorkflowNetwork
	}
	stepTimeout := time.Duration(workflow.StepTimeoutMS) * time.Millisecond
	if stepTimeout == 0 {
		stepTimeout = 5 * time.Second
	}
	for index, step := range workflow.Steps {
		stepCtx, cancel := context.WithTimeout(tab, stepTimeout)
		err := b.executeWorkflowStep(stepCtx, workflow, step)
		cancel()
		status := StatusPass
		if err != nil {
			status = StatusUnverified
			if errors.Is(err, ErrAssertionFailed) {
				status = StatusFail
			}
		}
		report.Steps = append(report.Steps, StepEvidence{Viewport: viewport, Index: index, Action: step.Action, Status: status})
		if err != nil {
			return err
		}
		var location string
		if err := chromedp.Run(tab, chromedp.Location(&location)); err != nil {
			return err
		}
		if b.workflowURL(tab, location) != nil || proxy.blocked.Load() {
			return errWorkflowSafety
		}
		if proxy.failed.Load() {
			return errWorkflowNetwork
		}
		if strings.HasPrefix(step.Action, "assert_") {
			report.Assertions++
		}
	}
	var sensitive bool
	if err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		document, err := dom.GetDocument().WithDepth(-1).WithPierce(true).Do(ctx)
		if err == nil {
			sensitive = sensitiveWorkflowNode(document)
		}
		return err
	})); err != nil {
		return err
	}
	if sensitive {
		return errWorkflowSafety
	}
	var screenshot []byte
	if err := chromedp.Run(tab,
		chromedp.Evaluate(`document.fonts.ready.then(() => new Promise(resolve => { const style = document.createElement('style'); style.textContent = '*,*::before,*::after { animation:none!important; transition:none!important; caret-color:transparent!important; }'; document.head.appendChild(style); requestAnimationFrame(() => requestAnimationFrame(resolve)); }))`, nil, func(params *runtime.EvaluateParams) *runtime.EvaluateParams { return params.WithAwaitPromise(true) }),
		chromedp.CaptureScreenshot(&screenshot)); err != nil {
		return err
	}
	if proxy.blocked.Load() {
		return errWorkflowSafety
	}
	if proxy.failed.Load() {
		return errWorkflowNetwork
	}
	if len(screenshot) > MaxScreenshotBytes {
		return errors.New("screenshot exceeds evidence budget")
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(screenshot))
	if err != nil || configuration.Width != int(width) || configuration.Height != int(height) {
		return errors.New("invalid screenshot dimensions")
	}
	name := viewport + ".png"
	root, err := os.OpenRoot(report.EvidenceDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := fsatomic.WriteFileRoot(root, name, screenshot, 0600); err != nil {
		return err
	}
	hash := sha256.Sum256(screenshot)
	report.Artifacts = append(report.Artifacts, Artifact{Name: name, Viewport: viewport, Bytes: len(screenshot), Width: configuration.Width, Height: configuration.Height, SHA256: hex.EncodeToString(hash[:])})
	return nil
}

func (b *Browser) executeWorkflowStep(ctx context.Context, workflow Workflow, step WorkflowStep) error {
	switch step.Action {
	case "navigate":
		return chromedp.Run(ctx, chromedp.Navigate(step.URL), chromedp.WaitReady("body", chromedp.ByQuery))
	case "click":
		return chromedp.Run(ctx, chromedp.Click(step.Selector, chromedp.ByQuery))
	case "fill":
		var nodes []*cdp.Node
		if err := chromedp.Run(ctx, chromedp.Nodes(step.Selector, &nodes, chromedp.ByQuery)); err != nil {
			return err
		}
		if len(nodes) != 1 {
			return errWorkflowSafety
		}
		attributes := make(map[string]string)
		for index := 0; index+1 < len(nodes[0].Attributes); index += 2 {
			attributes[nodes[0].Attributes[index]] = nodes[0].Attributes[index+1]
		}
		if !strings.EqualFold(nodes[0].NodeName, "input") && !strings.EqualFold(nodes[0].NodeName, "textarea") {
			return errWorkflowSafety
		}
		switch strings.ToLower(attributes["type"]) {
		case "", "text", "search", "email", "url", "number", "tel":
		default:
			return errWorkflowSafety
		}
		if strings.Contains(strings.ToLower(attributes["autocomplete"]), "password") {
			return errWorkflowSafety
		}
		return chromedp.Run(ctx, chromedp.SetValue(step.Selector, "", chromedp.ByQuery), chromedp.SendKeys(step.Selector, workflow.Fixtures[step.Fixture], chromedp.ByQuery))
	case "assert_visible":
		if err := chromedp.Run(ctx, chromedp.WaitVisible(step.Selector, chromedp.ByQuery)); err != nil {
			return workflowAssertionError(err)
		}
	case "assert_text":
		if err := chromedp.Run(ctx, chromedp.PollFunction(`(selector, expected) => { const element = document.querySelector(selector); return !!element && element.innerText.includes(expected); }`, nil, chromedp.WithPollingArgs(step.Selector, step.Text))); err != nil {
			return workflowAssertionError(err)
		}
	case "assert_url":
		if err := chromedp.Run(ctx, chromedp.PollFunction(`expected => location.href === expected`, nil, chromedp.WithPollingArgs(step.URL))); err != nil {
			return workflowAssertionError(err)
		}
	}
	return nil
}

func workflowAssertionError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, chromedp.ErrPollingTimeout) {
		return ErrAssertionFailed
	}
	return err
}

func sensitiveWorkflowNode(node *cdp.Node) bool {
	if node == nil {
		return false
	}
	if strings.EqualFold(node.NodeName, "iframe") || strings.EqualFold(node.NodeName, "frame") {
		return true
	}
	for index := 0; index+1 < len(node.Attributes); index += 2 {
		name, value := strings.ToLower(node.Attributes[index]), strings.ToLower(node.Attributes[index+1])
		if name == "type" && value == "password" || name == "autocomplete" && strings.Contains(value, "password") {
			return true
		}
	}
	for _, child := range append(append([]*cdp.Node{}, node.Children...), node.ShadowRoots...) {
		if sensitiveWorkflowNode(child) {
			return true
		}
	}
	return sensitiveWorkflowNode(node.ContentDocument)
}
