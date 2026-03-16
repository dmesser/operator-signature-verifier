package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containers/image/v5/docker"
	ctrImage "github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	godigest "github.com/opencontainers/go-digest"
)

// ──────────────────────────────────────────────────────────────
// Constants
// ──────────────────────────────────────────────────────────────

const (
	defaultWorkers = 5
	maxRetries     = 5
)

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
)

// ──────────────────────────────────────────────────────────────
// Types — FBC bundle extraction (jq output)
// ──────────────────────────────────────────────────────────────

type bundleExtract struct {
	Package       string         `json:"package"`
	Name          string         `json:"name"`
	Image         string         `json:"image"`
	Version       string         `json:"version"`
	RelatedImages []relatedImage `json:"relatedImages"`
}

type relatedImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// ──────────────────────────────────────────────────────────────
// Types — Image tracking & deduplication
// ──────────────────────────────────────────────────────────────

type imageEntry struct {
	Reference   string
	Appearances []imageAppearance
}

type imageAppearance struct {
	Package       string
	BundleName    string
	BundleVersion string
	ImageName     string
	OCPVersions   map[string]bool
}

// ──────────────────────────────────────────────────────────────
// Types — Verification results
// ──────────────────────────────────────────────────────────────

type verifyResult struct {
	Available bool
	AvailErr  string
	Signed    bool
	SignErr   string
	MultiArch bool
	Children  []childResult
}

type childResult struct {
	Arch   string
	OS     string
	Digest string
	Signed bool
	Err    string
}

// ──────────────────────────────────────────────────────────────
// Types — Progress display
// ──────────────────────────────────────────────────────────────

type progressUI struct {
	mu         sync.Mutex
	total      int
	numWorkers int
	completed  atomic.Int64
	avail      atomic.Int64
	unavail    atomic.Int64
	signed     atomic.Int64
	unsigned   atomic.Int64
	workers    []atomic.Value
	startTime  time.Time
	rendered   bool
}

func newProgress(total, numWorkers int) *progressUI {
	ui := &progressUI{
		total:      total,
		numWorkers: numWorkers,
		workers:    make([]atomic.Value, numWorkers),
		startTime:  time.Now(),
	}
	for i := range ui.workers {
		ui.workers[i].Store(fmt.Sprintf("%sidle%s", ansiDim, ansiReset))
	}
	return ui
}

func (ui *progressUI) lines() int { return 3 + ui.numWorkers }

func (ui *progressUI) setWorker(id int, text string) { ui.workers[id].Store(text) }

func (ui *progressUI) recordResult(available, signed bool) {
	ui.completed.Add(1)
	if available {
		ui.avail.Add(1)
		if signed {
			ui.signed.Add(1)
		} else {
			ui.unsigned.Add(1)
		}
	} else {
		ui.unavail.Add(1)
	}
}

func (ui *progressUI) render() {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	completed := int(ui.completed.Load())
	avail := int(ui.avail.Load())
	unavail := int(ui.unavail.Load())
	signed := int(ui.signed.Load())
	unsigned := int(ui.unsigned.Load())
	n := ui.lines()

	if ui.rendered {
		for i := 0; i < n; i++ {
			fmt.Fprint(os.Stderr, "\033[A\033[2K")
		}
	}
	ui.rendered = true

	pct := 0.0
	if ui.total > 0 {
		pct = float64(completed) / float64(ui.total) * 100
	}
	barWidth := 30
	filled := 0
	if ui.total > 0 {
		filled = int(float64(completed) / float64(ui.total) * float64(barWidth))
	}
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	elapsed := time.Since(ui.startTime)
	eta := "calculating..."
	if completed > 0 && completed < ui.total {
		rate := float64(elapsed) / float64(completed)
		remaining := time.Duration(rate * float64(ui.total-completed))
		eta = fmtDuration(remaining)
	} else if completed >= ui.total {
		eta = fmtDuration(elapsed)
	}

	fmt.Fprintf(os.Stderr, "  %sProgress%s  %s%s%s  %s%d%s/%-d  %s%.1f%%%s  ETA: %s\n",
		ansiBold, ansiReset, ansiGreen, bar, ansiReset,
		ansiBold, completed, ansiReset, ui.total,
		ansiCyan, pct, ansiReset, eta)
	fmt.Fprintf(os.Stderr, "  %sResults%s   %s✓%s %-d available  %s✗%s %-d unavail  %s✓%s %-d signed  %s✗%s %-d unsigned\n",
		ansiBold, ansiReset,
		ansiGreen, ansiReset, avail,
		ansiRed, ansiReset, unavail,
		ansiGreen, ansiReset, signed,
		ansiRed, ansiReset, unsigned)
	fmt.Fprintf(os.Stderr, "  %s──────────────────────────────────────────────────────────────────%s\n", ansiDim, ansiReset)

	for i := 0; i < ui.numWorkers; i++ {
		status := ui.workers[i].Load().(string)
		fmt.Fprintf(os.Stderr, "  %s⚙ W%-2d%s %s\n", ansiBlue, i+1, ansiReset, truncStr(status, 60))
	}
}

func (ui *progressUI) run(ctx context.Context) {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			ui.render()
			return
		case <-ticker.C:
			ui.render()
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────

func main() {
	catalogGlob := flag.String("catalogs", "", "Glob pattern for catalog files (default: redhat-operator-index-v4.*.json)")
	keyPath := flag.String("key", "red-hat-signing-key-v3.txt", "Path to the Red Hat signing public key (PEM)")
	output := flag.String("output", "verification-results.csv", "Output CSV file path")
	authFile := flag.String("authfile", "", "Path to registry auth.json (as used by skopeo/podman)")
	numWorkers := flag.Int("workers", defaultWorkers, "Number of parallel verification workers")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	printBanner()

	// ── Prerequisites ────────────────────────────────────────
	if _, err := exec.LookPath("jq"); err != nil {
		fatalf("jq not found in PATH (needed to parse catalog files)")
	}
	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		fatalf("Cannot read signing key %q: %v", *keyPath, err)
	}
	logStatus("✓", ansiGreen, "Signing key loaded from %s", *keyPath)

	// The containers/image library only fetches cosign-style sigstore
	// signatures (OCI tag sha256-<digest>.sig) when use-sigstore-attachments
	// is enabled in a registries.d config. Create a temporary config so that
	// the tool works without requiring system-wide configuration.
	regDir, err := os.MkdirTemp("", "opsigverify-registries-*")
	if err != nil {
		fatalf("Failed to create temp registries dir: %v", err)
	}
	defer os.RemoveAll(regDir)
	regCfg := []byte("default-docker:\n    use-sigstore-attachments: true\n")
	if err := os.WriteFile(filepath.Join(regDir, "sigstore.yaml"), regCfg, 0644); err != nil {
		fatalf("Failed to write registries config: %v", err)
	}

	sysCtx := &types.SystemContext{
		RegistriesDirPath: regDir,
	}
	if *authFile != "" {
		sysCtx.AuthFilePath = *authFile
		logStatus("✓", ansiGreen, "Auth file: %s", *authFile)
	}
	logStatus("✓", ansiGreen, "Sigstore attachment lookup enabled")

	// Validate that the key can produce a policy context (but don't reuse it
	// across goroutines — PolicyContext is not thread-safe).
	testPC, err := buildPolicyContext(keyData)
	if err != nil {
		fatalf("Failed to build signature policy: %v", err)
	}
	testPC.Destroy()
	logStatus("✓", ansiGreen, "Sigstore signature policy ready")
	fmt.Fprintln(os.Stderr)

	// ── Find catalogs ────────────────────────────────────────
	glob := *catalogGlob
	if glob == "" {
		glob = "redhat-operator-index-v4.*.json"
	}
	catalogs, _ := filepath.Glob(glob)
	if len(catalogs) == 0 {
		fatalf("No catalog files found matching %q", glob)
	}
	sort.Strings(catalogs)

	// ── Phase 1: Parse catalogs ──────────────────────────────
	logPhase(1, fmt.Sprintf("Parsing %d catalog(s)", len(catalogs)))
	fmt.Fprintln(os.Stderr)

	imageMap := make(map[string]*imageEntry)
	ocpRe := regexp.MustCompile(`v(\d+\.\d+)`)

	for _, cat := range catalogs {
		ocpVer := "unknown"
		if m := ocpRe.FindStringSubmatch(cat); m != nil {
			ocpVer = m[1]
		}
		logStatus("◉", ansiBlue, "Parsing %s (OCP %s)...", filepath.Base(cat), ocpVer)

		bundles, err := extractBundles(ctx, cat)
		if err != nil {
			logStatus("✗", ansiRed, "  Error: %v", err)
			continue
		}

		imgCount := 0
		for _, b := range bundles {
			version := b.Version
			if version == "" {
				version = versionFromName(b.Name)
			}
			for _, img := range collectImageRefs(b) {
				imgCount++
				addToImageMap(imageMap, img.ref, img.name, b.Package, b.Name, version, ocpVer)
			}
		}
		logStatus("✓", ansiGreen, "  %d bundles, %d image refs", len(bundles), imgCount)
	}

	tasks := sortedImageEntries(imageMap)
	fmt.Fprintln(os.Stderr)
	logStatus("▸", ansiGreen, "Total unique image references: %s%d%s", ansiBold, len(tasks), ansiReset)
	fmt.Fprintln(os.Stderr)

	// ── Phase 2: Verify images ───────────────────────────────
	logPhase(2, fmt.Sprintf("Verifying %d unique images (%d workers, %d retries max)", len(tasks), *numWorkers, maxRetries))
	fmt.Fprintln(os.Stderr)

	results := make(map[string]*verifyResult, len(tasks))
	var resultsMu sync.Mutex

	ui := newProgress(len(tasks), *numWorkers)
	uiCtx, uiCancel := context.WithCancel(ctx)
	var uiWg sync.WaitGroup
	uiWg.Add(1)
	go func() {
		defer uiWg.Done()
		ui.run(uiCtx)
	}()

	work := make(chan *imageEntry, *numWorkers*2)
	var wg sync.WaitGroup

	for w := 0; w < *numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workerPC, err := buildPolicyContext(keyData)
			if err != nil {
				logStatus("✗", ansiRed, "Worker %d failed to build policy context: %v", id, err)
				return
			}
			defer workerPC.Destroy()

			for entry := range work {
				if ctx.Err() != nil {
					return
				}
				first := entry.Appearances[0]
				desc := fmt.Sprintf("%s%s%s %sv%s%s → %s",
					ansiCyan, first.Package, ansiReset,
					ansiYellow, first.BundleVersion, ansiReset,
					truncRef(entry.Reference, 35))
				ui.setWorker(id, desc)

				res := verifyImageWithRetry(ctx, entry.Reference, workerPC, sysCtx)

				resultsMu.Lock()
				results[entry.Reference] = res
				resultsMu.Unlock()

				ui.recordResult(res.Available, res.Signed)
				ui.setWorker(id, fmt.Sprintf("%sidle%s", ansiDim, ansiReset))
			}
		}(w)
	}

	for _, task := range tasks {
		select {
		case <-ctx.Done():
		case work <- task:
		}
	}
	close(work)
	wg.Wait()

	uiCancel()
	uiWg.Wait()
	fmt.Fprintln(os.Stderr)

	// ── Phase 3: Write CSV & summary ─────────────────────────
	logPhase(3, "Writing results")

	if err := writeResultsCSV(*output, tasks, results); err != nil {
		logStatus("✗", ansiRed, "Error writing CSV: %v", err)
	} else {
		logStatus("✓", ansiGreen, "Results written to %s%s%s", ansiBold, *output, ansiReset)
	}

	fmt.Fprintln(os.Stderr)
	printSummary(ui)
}

// ──────────────────────────────────────────────────────────────
// Signature policy setup
// ──────────────────────────────────────────────────────────────

func buildPolicyContext(keyData []byte) (*signature.PolicyContext, error) {
	pr, err := signature.NewPRSigstoreSigned(
		signature.PRSigstoreSignedWithKeyData(keyData),
		signature.PRSigstoreSignedWithSignedIdentity(signature.NewPRMMatchRepository()),
	)
	if err != nil {
		return nil, fmt.Errorf("creating policy requirement: %w", err)
	}
	policy := &signature.Policy{
		Default:    signature.PolicyRequirements{pr},
		Transports: map[string]signature.PolicyTransportScopes{},
	}
	return signature.NewPolicyContext(policy)
}

// ──────────────────────────────────────────────────────────────
// Phase 1 — Catalog parsing via jq
// ──────────────────────────────────────────────────────────────

const jqFilter = `select(.schema == "olm.bundle") | {package, name, image, relatedImages: [(.relatedImages // [])[] | {name, image}], version: ([(.properties // [])[] | select(.type == "olm.package") | .value.version] | if length > 0 then .[0] else null end)}`

func extractBundles(ctx context.Context, catalogPath string) ([]bundleExtract, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "jq", "-c", jqFilter, catalogPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start jq: %w", err)
	}

	var bundles []bundleExtract
	dec := json.NewDecoder(stdout)
	for {
		var b bundleExtract
		if err := dec.Decode(&b); err != nil {
			break
		}
		bundles = append(bundles, b)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("jq: %w", err)
	}
	return bundles, nil
}

type imgRef struct {
	name string
	ref  string
}

func collectImageRefs(b bundleExtract) []imgRef {
	seen := make(map[string]bool)
	var result []imgRef

	for _, ri := range b.RelatedImages {
		if ri.Image == "" || seen[ri.Image] {
			continue
		}
		seen[ri.Image] = true
		name := ri.Name
		if name == "" {
			name = "bundle"
		}
		result = append(result, imgRef{name: name, ref: ri.Image})
	}
	if b.Image != "" && !seen[b.Image] {
		result = append(result, imgRef{name: "bundle", ref: b.Image})
	}
	return result
}

func addToImageMap(m map[string]*imageEntry, ref, imgName, pkg, bundleName, bundleVersion, ocpVer string) {
	entry, exists := m[ref]
	if !exists {
		entry = &imageEntry{Reference: ref}
		m[ref] = entry
	}
	for i, app := range entry.Appearances {
		if app.Package == pkg && app.BundleName == bundleName && app.ImageName == imgName {
			entry.Appearances[i].OCPVersions[ocpVer] = true
			return
		}
	}
	entry.Appearances = append(entry.Appearances, imageAppearance{
		Package:       pkg,
		BundleName:    bundleName,
		BundleVersion: bundleVersion,
		ImageName:     imgName,
		OCPVersions:   map[string]bool{ocpVer: true},
	})
}

func sortedImageEntries(m map[string]*imageEntry) []*imageEntry {
	entries := make([]*imageEntry, 0, len(m))
	for _, e := range m {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Reference < entries[j].Reference
	})
	return entries
}

// ──────────────────────────────────────────────────────────────
// Phase 2 — Image verification via containers/image
// ──────────────────────────────────────────────────────────────

func verifyImageWithRetry(ctx context.Context, ref string, pc *signature.PolicyContext, sys *types.SystemContext) *verifyResult {
	normalRef := normalizeRef(ref)
	imgRef, err := docker.ParseReference("//" + normalRef)
	if err != nil {
		return &verifyResult{AvailErr: fmt.Sprintf("parse ref: %v", err)}
	}

	var last *verifyResult
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(rand.Intn(500)) * time.Millisecond
			select {
			case <-ctx.Done():
				if last != nil {
					return last
				}
				return &verifyResult{AvailErr: ctx.Err().Error()}
			case <-time.After(backoff + jitter):
			}
		}
		last = doVerify(ctx, imgRef, pc, sys)
		if last.Available {
			return last
		}
	}
	return last
}

func doVerify(ctx context.Context, ref types.ImageReference, pc *signature.PolicyContext, sys *types.SystemContext) *verifyResult {
	result := &verifyResult{}

	src, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		result.AvailErr = err.Error()
		return result
	}
	defer src.Close()

	manifestBytes, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		result.AvailErr = err.Error()
		return result
	}
	result.Available = true

	if mimeType == "" {
		mimeType = manifest.GuessMIMEType(manifestBytes)
	}

	// Verify signature on the top-level manifest (works for both single
	// manifests and manifest lists where the signature is attached to the
	// list digest).
	topUnparsed := ctrImage.UnparsedInstance(src, nil)
	parentSigned, parentErr := pc.IsRunningImageAllowed(ctx, topUnparsed)

	if manifest.MIMETypeIsMultiImage(mimeType) {
		result.MultiArch = true
		list, err := manifest.ListFromBlob(manifestBytes, mimeType)
		if err != nil {
			result.SignErr = fmt.Sprintf("parse manifest list: %v", err)
			return result
		}

		result.Signed = parentSigned
		if !parentSigned && parentErr != nil {
			result.SignErr = parentErr.Error()
		}

		for _, d := range list.Instances() {
			info, err := list.Instance(d)
			if err != nil {
				continue
			}
			if info.ReadOnly.Platform == nil || info.ReadOnly.Platform.Architecture == "" {
				continue
			}

			cr := childResult{
				Arch:   info.ReadOnly.Platform.Architecture,
				OS:     info.ReadOnly.Platform.OS,
				Digest: d.String(),
			}

			if parentSigned {
				cr.Signed = true
			} else {
				childDigest := d
				childUnparsed := ctrImage.UnparsedInstance(src, &childDigest)
				childOK, childErr := pc.IsRunningImageAllowed(ctx, childUnparsed)
				cr.Signed = childOK
				if !childOK && childErr != nil {
					cr.Err = childErr.Error()
				}
			}
			result.Children = append(result.Children, cr)
		}

		if !parentSigned {
			all := true
			for _, c := range result.Children {
				if !c.Signed {
					all = false
					break
				}
			}
			result.Signed = all
		}
	} else {
		result.Signed = parentSigned
		if !parentSigned && parentErr != nil {
			result.SignErr = parentErr.Error()
		}
	}

	return result
}

// ──────────────────────────────────────────────────────────────
// Phase 3 — CSV output (multi-arch exploded to per-child rows)
// ──────────────────────────────────────────────────────────────

func writeResultsCSV(path string, entries []*imageEntry, results map[string]*verifyResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"package", "bundle_name", "bundle_version", "image_name",
		"image_reference", "ocp_versions",
		"available", "signed", "arch", "os", "child_digest", "error",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	type rowSource struct {
		pkg, bundleName, bundleVersion, imageName string
		ref                                       string
		ocpVersions                               map[string]bool
	}

	var sources []rowSource
	for _, entry := range entries {
		for _, app := range entry.Appearances {
			sources = append(sources, rowSource{
				pkg:           app.Package,
				bundleName:    app.BundleName,
				bundleVersion: app.BundleVersion,
				imageName:     app.ImageName,
				ref:           entry.Reference,
				ocpVersions:   app.OCPVersions,
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].pkg != sources[j].pkg {
			return sources[i].pkg < sources[j].pkg
		}
		if sources[i].bundleVersion != sources[j].bundleVersion {
			return sources[i].bundleVersion < sources[j].bundleVersion
		}
		return sources[i].imageName < sources[j].imageName
	})

	boolStr := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}

	for _, s := range sources {
		res := results[s.ref]
		if res == nil {
			continue
		}
		ocpStr := strings.Join(sortOCPVersions(s.ocpVersions), ";")

		if res.MultiArch && len(res.Children) > 0 {
			for _, child := range res.Children {
				errStr := child.Err
				if errStr == "" && !res.Available {
					errStr = res.AvailErr
				}
				row := []string{
					s.pkg, s.bundleName, s.bundleVersion, s.imageName,
					s.ref, ocpStr,
					boolStr(res.Available), boolStr(child.Signed),
					child.Arch, child.OS, child.Digest, truncStr(errStr, 200),
				}
				_ = w.Write(row)
			}
		} else {
			errStr := ""
			if !res.Available {
				errStr = res.AvailErr
			} else if !res.Signed {
				errStr = res.SignErr
			}
			row := []string{
				s.pkg, s.bundleName, s.bundleVersion, s.imageName,
				s.ref, ocpStr,
				boolStr(res.Available), boolStr(res.Signed),
				"", "", "", truncStr(errStr, 200),
			}
			_ = w.Write(row)
		}
	}

	return nil
}

// ──────────────────────────────────────────────────────────────
// Output helpers
// ──────────────────────────────────────────────────────────────

func printBanner() {
	fmt.Fprintf(os.Stderr, "\n%s%s══════════════════════════════════════════════════════════════%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Fprintf(os.Stderr, "%s%s  Red Hat Operator Signature Verifier%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Fprintf(os.Stderr, "%s%s══════════════════════════════════════════════════════════════%s\n\n", ansiBold, ansiCyan, ansiReset)
}

func logPhase(n int, msg string) {
	fmt.Fprintf(os.Stderr, "%s%s▸ Phase %d:%s %s\n", ansiBold, ansiBlue, n, ansiReset, msg)
}

func logStatus(icon, color, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "  %s%s%s %s\n", color, icon, ansiReset, msg)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  %s✗ %s%s\n", ansiRed, fmt.Sprintf(format, args...), ansiReset)
	os.Exit(1)
}

func printSummary(ui *progressUI) {
	total := int(ui.completed.Load())
	avail := int(ui.avail.Load())
	unavail := int(ui.unavail.Load())
	signed := int(ui.signed.Load())
	unsigned := int(ui.unsigned.Load())

	fmt.Fprintf(os.Stderr, "%s%s══════════════════════════════════════════════════════════════%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Fprintf(os.Stderr, "%s%s  Summary%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Fprintf(os.Stderr, "%s%s══════════════════════════════════════════════════════════════%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Fprintf(os.Stderr, "  Total images verified:  %s%d%s\n", ansiBold, total, ansiReset)
	fmt.Fprintf(os.Stderr, "  %s✓%s Available:           %d\n", ansiGreen, ansiReset, avail)
	fmt.Fprintf(os.Stderr, "  %s✗%s Unavailable:         %d\n", ansiRed, ansiReset, unavail)
	fmt.Fprintf(os.Stderr, "  %s✓%s Signed:              %d\n", ansiGreen, ansiReset, signed)
	fmt.Fprintf(os.Stderr, "  %s✗%s Unsigned:            %d\n", ansiRed, ansiReset, unsigned)
	fmt.Fprintf(os.Stderr, "  ⏱  Elapsed:             %s\n", fmtDuration(time.Since(ui.startTime)))
	fmt.Fprintf(os.Stderr, "%s%s══════════════════════════════════════════════════════════════%s\n\n", ansiBold, ansiCyan, ansiReset)
}

// ──────────────────────────────────────────────────────────────
// Utilities
// ──────────────────────────────────────────────────────────────

// normalizeRef strips the tag from refs that contain both a tag and a digest,
// e.g. "registry.io/img:tag@sha256:abc" → "registry.io/img@sha256:abc".
// Some registries and older catalog entries encode both; the digest is
// authoritative and many tools (including skopeo) reject the dual form.
func normalizeRef(ref string) string {
	atIdx := strings.Index(ref, "@")
	if atIdx == -1 {
		return ref
	}
	repo := ref[:atIdx]
	digestPart := ref[atIdx:]
	lastSlash := strings.LastIndex(repo, "/")
	lastColon := strings.LastIndex(repo, ":")
	if lastColon > lastSlash {
		repo = repo[:lastColon]
	}
	return repo + digestPart
}

var versionRe = regexp.MustCompile(`\.v?(\d+\.\d+\.\d+.*)$`)

func versionFromName(name string) string {
	if m := versionRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

func repoFromRef(ref string) string {
	if idx := strings.LastIndex(ref, "@"); idx != -1 {
		return ref[:idx]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon]
	}
	return ref
}

func truncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

func truncRef(ref string, maxLen int) string {
	if len(ref) <= maxLen {
		return ref
	}
	return "…" + ref[len(ref)-maxLen+1:]
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func sortOCPVersions(versions map[string]bool) []string {
	sorted := make([]string, 0, len(versions))
	for v := range versions {
		sorted = append(sorted, v)
	}
	sort.Slice(sorted, func(i, j int) bool {
		partsI := strings.SplitN(sorted[i], ".", 2)
		partsJ := strings.SplitN(sorted[j], ".", 2)
		if len(partsI) == 2 && len(partsJ) == 2 && partsI[0] == partsJ[0] {
			mi, _ := strconv.Atoi(partsI[1])
			mj, _ := strconv.Atoi(partsJ[1])
			return mi < mj
		}
		return sorted[i] < sorted[j]
	})
	return sorted
}

// Ensure the digest import is used (it is referenced in doVerify via
// list.Instances() which returns []godigest.Digest).
var _ godigest.Digest
