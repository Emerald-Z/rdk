package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/multierr"
	pb "go.viam.com/api/app/packages/v1"
	"go.viam.com/utils"

	"go.viam.com/rdk/config"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	rutils "go.viam.com/rdk/utils"
)

const (
	allowedContentType = "application/x-gzip"
)

var (
	_ Manager       = (*cloudManager)(nil)
	_ ManagerSyncer = (*cloudManager)(nil)
)

type cloudManager struct {
	resource.Named
	// we assume the config is immutable for the lifetime of the process
	resource.TriviallyReconfigurable
	client          pb.PackageServiceClient
	httpClient      http.Client
	packagesDataDir string
	packagesDir     string
	cloudConfig     config.Cloud

	managedPackages map[PackageName]*config.PackageConfig
	mu              sync.RWMutex

	logger logging.Logger
}

// SubtypeName is a constant that identifies the internal package manager resource subtype string.
const SubtypeName = "packagemanager"

// API is the fully qualified API for the internal package manager service.
var API = resource.APINamespaceRDKInternal.WithServiceType(SubtypeName)

// InternalServiceName is used to refer to/depend on this service internally.
var InternalServiceName = resource.NewName(API, "builtin")

// NewCloudManager creates a new manager with the given package service client and directory to sync to.
func NewCloudManager(
	cloudConfig *config.Cloud,
	client pb.PackageServiceClient,
	packagesDir string,
	logger logging.Logger,
) (ManagerSyncer, error) {
	packagesDataDir := filepath.Join(packagesDir, "data")

	if err := os.MkdirAll(packagesDir, 0o700); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(packagesDataDir, 0o700); err != nil {
		return nil, err
	}

	return &cloudManager{
		Named:           InternalServiceName.AsNamed(),
		client:          client,
		httpClient:      http.Client{Timeout: time.Minute * 30},
		cloudConfig:     *cloudConfig,
		packagesDir:     packagesDir,
		packagesDataDir: packagesDataDir,
		logger:          logger,
	}, nil
}

// PackagePath returns the package if it exists and is already downloaded. If it does not exist it returns a ErrPackageMissing error.
func (m *cloudManager) PackagePath(name PackageName) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.managedPackages[name]
	if !ok {
		return "", ErrPackageMissing
	}

	return p.LocalDataDirectory(m.packagesDir), nil
}

// Close manager.
func (m *cloudManager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.httpClient.CloseIdleConnections()
	return nil
}

// SyncAll syncs all given packages and removes any not in the list from the local file system.
func (m *cloudManager) Sync(ctx context.Context, packages []config.PackageConfig, modules []config.Module) error {
	var outErr error

	// Only allow one rdk process to operate on the manager at once. This is generally safe to keep locked for an extended period of time
	// since the config reconfiguration process is handled by a single thread.
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Debug("Evaluating package sync...")

	newManagedPackages := make(map[PackageName]*config.PackageConfig, len(packages))

	// Process the packages that are new or changed
	changedPackages, existingPackages := m.validateAndGetChangedPackages(packages)

	// Updated managed map with existingPackages
	for _, p := range existingPackages {
		p := p
		newManagedPackages[PackageName(p.Name)] = &p
	}

	if len(changedPackages) > 0 {
		m.logger.Info("Package changes have been detected, starting sync")
	}

	start := time.Now()
	for idx, p := range changedPackages {
		p := p
		pkgStart := time.Now()
		if err := ctx.Err(); err != nil {
			m.logger.Errorf("Context canceled. Canceling cloud package manager sync. Time spent: %v", time.Since(start))
			return multierr.Append(outErr, err)
		}

		m.logger.Debugf("Starting package sync [%d/%d] %s:%s", idx+1, len(changedPackages), p.Package, p.Version)

		// Lookup the packages http url
		includeURL := true

		packageType, err := config.PackageTypeToProto(p.Type)
		if err != nil {
			m.logger.Warnw("failed to get package type", "package", p.Name, "error", err)
		}

		resp, err := m.client.GetPackage(ctx, &pb.GetPackageRequest{
			Id:         p.Package,
			Version:    p.Version,
			Type:       packageType,
			IncludeUrl: &includeURL,
		})
		if err != nil {
			m.logger.Errorf("Failed fetching package details for package %s:%s. Err: %v", p.Package, p.Version, err)
			outErr = multierr.Append(outErr, fmt.Errorf("failed loading package url for %s:%s %w", p.Package, p.Version, err))
			continue
		}

		m.logger.Debugf("Downloading from %s", sanitizeURLForLogs(resp.Package.Url))

		// download package from a http endpoint
		err = installPackage(ctx, m.logger, m.packagesDir, resp.Package.Url, p,
			func(ctx context.Context, url, dstPath string) (string, string, error) {
				statusFile := packageSyncFile{
					PackageID:       p.Package,
					Version:         p.Version,
					ModifiedTime:    time.Now(),
					Status:          syncStatusDownloading,
					TarballChecksum: "",
				}

				err = writeStatusFile(p, statusFile, m.packagesDir)
				if err != nil {
					return "", "", err
				}

				return m.downloadFileFromGCSURL(ctx, url, dstPath, m.cloudConfig.ID, m.cloudConfig.Secret)
			},
		)
		if err != nil {
			m.logger.Errorf("Failed downloading package %s:%s from %s, %s", p.Package, p.Version, sanitizeURLForLogs(resp.Package.Url), err)
			outErr = multierr.Append(outErr, fmt.Errorf("failed downloading package %s:%s from %s %w",
				p.Package, p.Version, sanitizeURLForLogs(resp.Package.Url), err))
			continue
		}

		if p.Type == config.PackageTypeMlModel {
			if symlinkErr := m.mLModelSymlinkCreation(p); symlinkErr != nil {
				m.logger.Errorf("Error creating ml model symlink. Err: %v", symlinkErr)
				outErr = multierr.Append(outErr, symlinkErr)
			}
		}

		// add to managed packages
		newManagedPackages[PackageName(p.Name)] = &p

		m.logger.Debugf("Package sync complete [%d/%d] %s:%s after %v", idx+1, len(changedPackages), p.Package, p.Version, time.Since(pkgStart))
	}

	if len(changedPackages) > 0 {
		m.logger.Infof("Package sync complete after %v", time.Since(start))
	}

	// swap for new managed packags.
	m.managedPackages = newManagedPackages

	return outErr
}

func (m *cloudManager) validateAndGetChangedPackages(
	packages []config.PackageConfig,
) ([]config.PackageConfig, []config.PackageConfig) {
	changed := make([]config.PackageConfig, 0)
	existing := make([]config.PackageConfig, 0)
	for _, p := range packages {
		// don't consider invalid config as synced or unsynced
		if err := p.Validate(""); err != nil {
			m.logger.Errorw("package config validation error; skipping", "package", p.Name, "error", err)
			continue
		}
		newPackage := p
		if packageIsSynced(p, m.packagesDir, m.logger) {
			existing = append(existing, newPackage)
			m.logger.Debugf("Package %s:%s already managed, skipping", p.Package, p.Version)
		} else {
			changed = append(changed, newPackage)
		}
	}
	return changed, existing
}

// Cleanup removes all unknown packages from the working directory.
func (m *cloudManager) Cleanup(ctx context.Context) error {
	// Only allow one rdk process to operate on the manager at once. This is generally safe to keep locked for an extended period of time
	// since the config reconfiguration process is handled by a single thread.
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Debug("Starting package cleanup...")

	var allErrors error

	expectedPackageDirectories := map[string]bool{}
	for _, pkg := range m.managedPackages {
		expectedPackageDirectories[pkg.LocalDataDirectory(m.packagesDir)] = true
	}

	allErrors = commonCleanup(m.logger, expectedPackageDirectories, m.packagesDataDir)
	if allErrors != nil {
		return allErrors
	}

	allErrors = multierr.Append(allErrors, m.mlModelSymlinkCleanup())
	return allErrors
}

// symlink packages/package-name to packages/data/ml_model/orgid-package-name-ver for backwards compatibility
// TODO(RSDK-4386) Preserved for backwards compatibility. Could be removed or extended to other types in the future.
func (m *cloudManager) mLModelSymlinkCreation(p config.PackageConfig) error {
	symlinkPath, err := rutils.SafeJoinDir(m.packagesDir, p.Name)
	if err != nil {
		return err
	}

	localDataDir := p.LocalDataDirectory(m.packagesDir)
	if err := linkFile(localDataDir, symlinkPath); err != nil {
		return fmt.Errorf("failed linking ml_model package %s:%s. localDataDir: %q symlinkPath: %q Err: %w",
			p.Package, p.Version, localDataDir, symlinkPath, err)
	}
	return nil
}

// cleanup all symlinks in packages/ directory
// TODO(RSDK-4386) Preserved for backwards compatibility. Could be removed or extended to other types in the future.
func (m *cloudManager) mlModelSymlinkCleanup() error {
	var allErrors error
	files, err := os.ReadDir(m.packagesDir)
	if err != nil {
		return err
	}

	// The only symlinks in this directory are those created for MLModels
	for _, f := range files {
		if f.Type()&os.ModeSymlink != os.ModeSymlink {
			continue
		}
		// if managed skip removing package
		if _, ok := m.managedPackages[PackageName(f.Name())]; ok {
			continue
		}

		m.logger.Infof("Cleaning up unused package link %s", f.Name())

		symlinkPath, err := rutils.SafeJoinDir(m.packagesDir, f.Name())
		if err != nil {
			allErrors = multierr.Append(allErrors, err)
			continue
		}
		// Remove logical symlink to package
		if err := os.Remove(symlinkPath); err != nil {
			allErrors = multierr.Append(allErrors, err)
		}
	}
	return allErrors
}

func sanitizeURLForLogs(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	parsed.RawQuery = ""
	return parsed.String()
}

// LogProgressWriter is a writer that logs progress.
type logProgressWriter struct {
	totalWrittenBytes int64
	totalBytes        int64
	name              string
	startTime         time.Time
	lastLogTime       time.Time
	logFrequency      time.Duration
	logger            logging.Logger
}

func newLogProgressWriter(logger logging.Logger, jobName string, totalBytes int64) *logProgressWriter {
	return &logProgressWriter{
		totalWrittenBytes: 0,
		totalBytes:        totalBytes,
		startTime:         time.Now(),
		lastLogTime:       time.Now(),
		name:              jobName,
		logFrequency:      time.Second * 5,
		logger:            logger,
	}
}

func (wc *logProgressWriter) Write(p []byte) (int, error) {
	currentTime := time.Now()
	bytesWritten := len(p)
	wc.totalWrittenBytes += int64(bytesWritten)

	if wc.totalWrittenBytes < wc.totalBytes || wc.totalBytes == 0 {
		if currentTime.Sub(wc.lastLogTime) > wc.logFrequency {
			// unknown pct if totalBytes is 0. Log what's available.
			var pctStr string
			if wc.totalBytes == 0 {
				pctStr = "?%"
			} else {
				pctStr = fmt.Sprintf("%.0f%%", float64(wc.totalWrittenBytes)/float64(wc.totalBytes)*100)
			}
			// Prevent NPE if logFrequency is also 0. non-exact is fine for display purposes
			if currentTime == wc.startTime {
				currentTime = currentTime.Add(time.Second)
			}
			wc.logger.Infof("%s: downloaded %d / %d bytes (%s) [%.0f KB/s]",
				wc.name,
				wc.totalWrittenBytes,
				wc.totalBytes,
				pctStr,
				float64(wc.totalWrittenBytes)/currentTime.Sub(wc.startTime).Seconds()/1024)
			wc.lastLogTime = currentTime
		}
	} else {
		wc.logger.Infof("%s: downloaded %d bytes (100%%) in %v",
			wc.name,
			wc.totalWrittenBytes,
			currentTime.Sub(wc.startTime))
	}
	return bytesWritten, nil
}

func (m *cloudManager) downloadFileFromGCSURL(
	ctx context.Context,
	url string,
	downloadPath string,
	partID string,
	partSecret string,
) (string, string, error) {
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	getReq.Header.Add("part_id", partID)
	getReq.Header.Add("secret", partSecret)
	if err != nil {
		return "", "", err
	}

	//nolint:bodyclose /// closed in UncheckedErrorFunc
	resp, err := m.httpClient.Do(getReq)
	if err != nil {
		return "", "", err
	}
	defer utils.UncheckedErrorFunc(resp.Body.Close)

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("invalid status code %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	checksum := getGoogleHash(resp.Header, "crc32c")

	//nolint:gosec // safe
	out, err := os.Create(downloadPath)
	if err != nil {
		return checksum, contentType, err
	}
	defer utils.UncheckedErrorFunc(out.Close)

	hash := crc32Hash()
	w := io.MultiWriter(out, hash, newLogProgressWriter(m.logger, downloadPath, resp.ContentLength))

	_, err = io.CopyN(w, resp.Body, maxPackageSize)
	if err != nil && !errors.Is(err, io.EOF) {
		utils.UncheckedError(os.Remove(downloadPath))
		return checksum, contentType, err
	}

	checksumBytes, err := base64.StdEncoding.DecodeString(checksum)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode expected checksum: %s %w", checksum, err)
	}

	trimmedChecksumBytes := trimLeadingZeroes(checksumBytes)
	trimmedOutHashBytes := trimLeadingZeroes(hash.Sum(nil))

	if !bytes.Equal(trimmedOutHashBytes, trimmedChecksumBytes) {
		utils.UncheckedError(os.Remove(downloadPath))
		return checksum, contentType, fmt.Errorf(
			"download did not match expected hash:\n"+
				"  pre-trimmed: %x vs. %x\n"+
				"  trimmed:     %x vs. %x",
			checksumBytes, hash.Sum(nil),
			trimmedChecksumBytes, trimmedOutHashBytes,
		)
	}

	return checksum, contentType, nil
}

func trimLeadingZeroes(data []byte) []byte {
	if len(data) == 0 {
		return []byte{}
	}

	for i, b := range data {
		if b != 0x00 {
			return data[i:]
		}
	}

	// If all bytes are zero, return a single zero byte
	return []byte{0x00}
}

func getGoogleHash(headers http.Header, hashType string) string {
	hashes := headers.Values("x-goog-hash")
	hashesMap := make(map[string]string, len(hashes))
	for _, v := range hashes {
		split := strings.SplitN(v, "=", 2)
		if len(split) != 2 {
			continue
		}
		hashesMap[split[0]] = split[1]
	}

	return hashesMap[hashType]
}

func crc32Hash() hash.Hash32 {
	return crc32.New(crc32.MakeTable(crc32.Castagnoli))
}

func safeLink(parent, link string) (string, error) {
	if filepath.IsAbs(link) {
		return link, fmt.Errorf("unsafe path link: '%s' with '%s', cannot be absolute path", parent, link)
	}

	_, err := rutils.SafeJoinDir(parent, link)
	if err != nil {
		return link, err
	}
	return link, nil
}

func linkFile(from, to string) error {
	link, err := os.Readlink(to)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if link == from {
		return nil
	}

	// remove any existing link or SymLink will fail.
	if link != "" {
		utils.UncheckedError(os.Remove(from))
	}

	return os.Symlink(from, to)
}

// SyncOne is a no-op for cloudManager.
func (m *cloudManager) SyncOne(ctx context.Context, mod config.Module) error {
	return nil
}
