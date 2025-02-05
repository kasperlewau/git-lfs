package tq

import (
	"strings"
	"sync"

	"github.com/git-lfs/git-lfs/v3/config"
	"github.com/git-lfs/git-lfs/v3/fs"
	"github.com/git-lfs/git-lfs/v3/lfsapi"
	"github.com/git-lfs/git-lfs/v3/ssh"
	"github.com/rubyist/tracerx"
)

const (
	defaultMaxRetries          = 8
	defaultMaxRetryDelay       = 10
	defaultConcurrentTransfers = 8
)

type Manifest struct {
	// maxRetries is the maximum number of retries a single object can
	// attempt to make before it will be dropped. maxRetryDelay is the maximum
	// time in seconds to wait between retry attempts when using backoff.
	maxRetries              int
	maxRetryDelay           int
	concurrentTransfers     int
	basicTransfersOnly      bool
	standaloneTransferAgent string
	tusTransfersAllowed     bool
	downloadAdapterFuncs    map[string]NewAdapterFunc
	uploadAdapterFuncs      map[string]NewAdapterFunc
	fs                      *fs.Filesystem
	apiClient               *lfsapi.Client
	sshTransfer             *ssh.SSHTransfer
	batchClientAdapter      BatchClient
	mu                      sync.Mutex
}

func (m *Manifest) APIClient() *lfsapi.Client {
	return m.apiClient
}

func (m *Manifest) MaxRetries() int {
	return m.maxRetries
}

func (m *Manifest) MaxRetryDelay() int {
	return m.maxRetryDelay
}

func (m *Manifest) ConcurrentTransfers() int {
	return m.concurrentTransfers
}

func (m *Manifest) IsStandaloneTransfer() bool {
	return m.standaloneTransferAgent != ""
}

func (m *Manifest) batchClient() BatchClient {
	if r := m.MaxRetries(); r > 0 {
		m.batchClientAdapter.SetMaxRetries(r)
	}
	return m.batchClientAdapter
}

func NewManifest(f *fs.Filesystem, apiClient *lfsapi.Client, operation, remote string) *Manifest {
	if apiClient == nil {
		cli, err := lfsapi.NewClient(nil)
		if err != nil {
			tracerx.Printf("unable to init tq.Manifest: %s", err)
			return nil
		}
		apiClient = cli
	}

	sshTransfer := apiClient.SSHTransfer(operation, remote)

	m := &Manifest{
		fs:                   f,
		apiClient:            apiClient,
		batchClientAdapter:   &tqClient{Client: apiClient},
		downloadAdapterFuncs: make(map[string]NewAdapterFunc),
		uploadAdapterFuncs:   make(map[string]NewAdapterFunc),
		sshTransfer:          sshTransfer,
	}

	var tusAllowed bool
	if git := apiClient.GitEnv(); git != nil {
		if v := git.Int("lfs.transfer.maxretries", 0); v > 0 {
			m.maxRetries = v
		}
		if v := git.Int("lfs.transfer.maxretrydelay", -1); v > -1 {
			m.maxRetryDelay = v
		}
		if v := git.Int("lfs.concurrenttransfers", 0); v > 0 {
			m.concurrentTransfers = v
		}
		m.basicTransfersOnly = git.Bool("lfs.basictransfersonly", false)
		m.standaloneTransferAgent = findStandaloneTransfer(
			apiClient, operation, remote,
		)
		tusAllowed = git.Bool("lfs.tustransfers", false)
		configureCustomAdapters(git, m)
	}

	if m.maxRetries < 1 {
		m.maxRetries = defaultMaxRetries
	}
	if m.maxRetryDelay < 1 {
		m.maxRetryDelay = defaultMaxRetryDelay
	}

	if m.concurrentTransfers < 1 {
		m.concurrentTransfers = defaultConcurrentTransfers
	}

	if sshTransfer != nil {
		// Multiple concurrent transfers are not yet supported.
		m.batchClientAdapter = &SSHBatchClient{
			maxRetries: m.maxRetries,
			transfer:   sshTransfer,
		}
	}

	configureBasicDownloadAdapter(m)
	configureBasicUploadAdapter(m)
	if tusAllowed {
		configureTusAdapter(m)
	}
	configureSSHAdapter(m)
	return m
}

func findDefaultStandaloneTransfer(url string) string {
	if strings.HasPrefix(url, "file://") {
		return standaloneFileName
	}
	return ""
}

func findStandaloneTransfer(client *lfsapi.Client, operation, remote string) string {
	if operation == "" || remote == "" {
		v, _ := client.GitEnv().Get("lfs.standalonetransferagent")
		return v
	}

	ep := client.Endpoints.Endpoint(operation, remote)
	aep := client.Endpoints.Endpoint(operation, remote)
	uc := config.NewURLConfig(client.GitEnv())
	v, ok := uc.Get("lfs", ep.Url, "standalonetransferagent")
	if !ok {
		return findDefaultStandaloneTransfer(aep.Url)
	}

	return v
}

// GetAdapterNames returns a list of the names of adapters available to be created
func (m *Manifest) GetAdapterNames(dir Direction) []string {
	switch dir {
	case Upload:
		return m.GetUploadAdapterNames()
	case Download:
		return m.GetDownloadAdapterNames()
	}
	return nil
}

// GetDownloadAdapterNames returns a list of the names of download adapters available to be created
func (m *Manifest) GetDownloadAdapterNames() []string {
	return m.getAdapterNames(m.downloadAdapterFuncs)
}

// GetUploadAdapterNames returns a list of the names of upload adapters available to be created
func (m *Manifest) GetUploadAdapterNames() []string {
	return m.getAdapterNames(m.uploadAdapterFuncs)
}

// getAdapterNames returns a list of the names of adapters available to be created
func (m *Manifest) getAdapterNames(adapters map[string]NewAdapterFunc) []string {
	if m.basicTransfersOnly {
		return []string{BasicAdapterName}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ret := make([]string, 0, len(adapters))
	for n, _ := range adapters {
		ret = append(ret, n)
	}
	return ret
}

// RegisterNewTransferAdapterFunc registers a new function for creating upload
// or download adapters. If a function with that name & direction is already
// registered, it is overridden
func (m *Manifest) RegisterNewAdapterFunc(name string, dir Direction, f NewAdapterFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch dir {
	case Upload:
		m.uploadAdapterFuncs[name] = f
	case Download:
		m.downloadAdapterFuncs[name] = f
	}
}

// Create a new adapter by name and direction; default to BasicAdapterName if doesn't exist
func (m *Manifest) NewAdapterOrDefault(name string, dir Direction) Adapter {
	if len(name) == 0 {
		name = BasicAdapterName
	}

	a := m.NewAdapter(name, dir)
	if a == nil {
		tracerx.Printf("Defaulting to basic transfer adapter since %q did not exist", name)
		a = m.NewAdapter(BasicAdapterName, dir)
	}
	return a
}

// Create a new adapter by name and direction, or nil if doesn't exist
func (m *Manifest) NewAdapter(name string, dir Direction) Adapter {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch dir {
	case Upload:
		if u, ok := m.uploadAdapterFuncs[name]; ok {
			return u(name, dir)
		}
	case Download:
		if d, ok := m.downloadAdapterFuncs[name]; ok {
			return d(name, dir)
		}
	}
	return nil
}

// Create a new download adapter by name, or BasicAdapterName if doesn't exist
func (m *Manifest) NewDownloadAdapter(name string) Adapter {
	return m.NewAdapterOrDefault(name, Download)
}

// Create a new upload adapter by name, or BasicAdapterName if doesn't exist
func (m *Manifest) NewUploadAdapter(name string) Adapter {
	return m.NewAdapterOrDefault(name, Upload)
}

// Env is any object with a config.Environment interface.
type Env interface {
	Get(key string) (val string, ok bool)
	GetAll(key string) []string
	Bool(key string, def bool) (val bool)
	Int(key string, def int) (val int)
	All() map[string][]string
}
