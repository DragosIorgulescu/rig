package infrastructure

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BaronBonet/rig/internal/adapters/client/claude"
	"github.com/BaronBonet/rig/internal/adapters/client/codex"
	"github.com/BaronBonet/rig/internal/adapters/repository/userconfig"
	"github.com/BaronBonet/rig/internal/adapters/taskdaemon"
	"github.com/BaronBonet/rig/internal/core"

	sqlite "github.com/BaronBonet/rig/internal/adapters/repository/sqlite"

	"github.com/caarlos0/env/v11"
)

type ApplicationConfig struct {
	// Provider is the optional runtime default-provider override
	// (RIG_PROVIDER). The user's default provider normally comes from
	// provider setup in user-level config; when set, this override must name a
	// configured provider.
	Provider   core.Provider `env:"RIG_PROVIDER"`
	SQLite     sqlite.Config
	Codex      codex.Config
	Claude     claude.Config
	UserConfig userconfig.Config
	Daemon     taskdaemon.Config
	Scheduler  SchedulerConfig
}

// SchedulerConfig paces scheduled tasks. A sandbox shortens both to try
// schedules without waiting minutes.
type SchedulerConfig struct {
	// Tick is how often the daemon looks for scheduled runs that are due
	// (RIG_SCHEDULER_TICK).
	Tick time.Duration `env:"RIG_SCHEDULER_TICK"         envDefault:"30s"`
	// MinInterval is the shortest time allowed between two runs of a schedule
	// (RIG_SCHEDULER_MIN_INTERVAL).
	MinInterval time.Duration `env:"RIG_SCHEDULER_MIN_INTERVAL" envDefault:"5m"`
}

// LoadConfig loads the application configuration from environment variables.
func LoadConfig() (*ApplicationConfig, error) {
	config := ApplicationConfig{
		SQLite: sqlite.Config{
			Path: sqlite.DefaultSQLitePath(),
		},
		Daemon: taskdaemon.Config{
			SocketPath: defaultDaemonSocketPath(),
		},
	}

	if err := env.Parse(&config); err != nil {
		return nil, err
	}
	if err := validateProviderOverride(config.Provider); err != nil {
		return nil, err
	}
	if err := config.Scheduler.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func (c SchedulerConfig) validate() error {
	if c.Tick <= 0 {
		return fmt.Errorf("invalid RIG_SCHEDULER_TICK %s: expected a positive duration such as 30s", c.Tick)
	}
	if c.MinInterval <= 0 {
		return fmt.Errorf(
			"invalid RIG_SCHEDULER_MIN_INTERVAL %s: expected a positive duration such as 5m",
			c.MinInterval,
		)
	}
	return nil
}

func validateProviderOverride(provider core.Provider) error {
	if provider == "" || core.IsSupportedProvider(provider) {
		return nil
	}

	supported := make([]string, 0, len(core.SupportedProviders()))
	for _, name := range core.SupportedProviders() {
		supported = append(supported, string(name))
	}
	return fmt.Errorf("invalid RIG_PROVIDER %q: expected one of %s", provider, strings.Join(supported, ", "))
}

func defaultDaemonSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".rig/daemon.sock"
	}

	return filepath.Join(home, ".local", "share", "rig", "daemon.sock")
}
