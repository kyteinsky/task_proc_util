// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config parses Nextcloud's config.php and exposes the values the
// supervisor needs, following the same pattern as notify_push.
package config

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Config is the immutable startup configuration derived from config.php.
type Config struct {
	// DBDriver is "mysql", "postgres", or "sqlite3".
	DBDriver string
	// DBDSN is the driver-specific data source name.
	DBDSN string
	// DBPrefix is the Nextcloud table prefix, e.g. "oc_".
	DBPrefix string
	// NextcloudURL is derived from overwrite.cli.url in config.php.
	NextcloudURL string
	// OccCommand is the argv used to invoke occ.
	OccCommand []string
	// OccDir is the Nextcloud root directory that occ must be run from.
	OccDir string
	// WorkerTimeout is how long (seconds) each worker runs before recycling.
	WorkerTimeout int
}

// FromArgs parses CLI args and loads config.php.
//
// Usage:  task_proc_util-supervisor [flags] /path/to/config.php
//
// Flags override values derived from config.php (same precedence as notify_push).
func FromArgs(args []string) (*Config, error) {
	fs := flag.NewFlagSet("task_proc_util-supervisor", flag.ContinueOnError)

	occ := fs.String("occ", env("NC_OCC", "php occ"), "occ command (space-separated)")
	timeout := fs.Int("worker-timeout", envInt("NC_WORKER_TIMEOUT", 300), "Worker recycle timeout in seconds (0 = never)")
	occDir := fs.String("occ-dir", env("NC_OCC_DIR", ""), "Nextcloud root directory to run occ from (default: parent of config.php's directory)")
	dbURL := fs.String("database-url", env("DATABASE_URL", ""), "Override database DSN (driver://...)")
	dbPrefix := fs.String("database-prefix", env("DATABASE_PREFIX", ""), "Override table prefix")
	ncURL := fs.String("nextcloud-url", env("NEXTCLOUD_URL", ""), "Override Nextcloud URL")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &Config{
		DBPrefix:      "oc_",
		WorkerTimeout: *timeout,
	}

	// Load config.php if provided as positional argument or via env.
	configFile := fs.Arg(0)
	if configFile == "" {
		configFile = env("NC_CONFIG_FILE", "")
	}
	if configFile != "" {
		if err := cfg.loadFromFile(configFile); err != nil {
			return nil, fmt.Errorf("reading config.php: %w", err)
		}
		// config.php lives at <nextcloud root>/config/config.php, so the root
		// is two levels up. occ must be invoked from there.
		if abs, err := filepath.Abs(configFile); err == nil {
			cfg.OccDir = filepath.Dir(filepath.Dir(abs))
		}
	}

	// CLI/env flags override config.php values.
	if *dbURL != "" {
		driver, dsn, err := parseDatabaseURL(*dbURL)
		if err != nil {
			return nil, err
		}
		cfg.DBDriver = driver
		cfg.DBDSN = dsn
	}
	if *dbPrefix != "" {
		cfg.DBPrefix = *dbPrefix
	}
	if *ncURL != "" {
		cfg.NextcloudURL = strings.TrimRight(*ncURL, "/")
	}
	if *occDir != "" {
		cfg.OccDir = *occDir
	}

	occParts := strings.Fields(*occ)
	if len(occParts) == 0 {
		return nil, fmt.Errorf("occ command is empty")
	}
	cfg.OccCommand = occParts

	if cfg.DBDriver == "" {
		return nil, fmt.Errorf("no database configured (pass config.php as argument or --database-url)")
	}
	if cfg.NextcloudURL == "" {
		return nil, fmt.Errorf("no Nextcloud URL configured (pass config.php as argument or --nextcloud-url)")
	}

	return cfg, nil
}

// phpStringRe matches  'key' => 'value'  lines in $CONFIG array.
var phpStringRe = regexp.MustCompile(`['"]([^'"]+)['"]\s*=>\s*['"]([^'"]*?)['"]`)

// loadFromFile reads the PHP $CONFIG array and populates cfg.
func (c *Config) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	kv := make(map[string]string)
	for _, m := range phpStringRe.FindAllStringSubmatch(string(data), -1) {
		kv[m[1]] = m[2]
	}

	dbtype := strings.ToLower(kv["dbtype"])
	switch dbtype {
	case "mysql", "mariadb":
		c.DBDriver = "mysql"
		c.DBDSN = buildMySQLDSN(kv)
	case "pgsql":
		c.DBDriver = "postgres"
		c.DBDSN = buildPostgresDSN(kv)
	case "sqlite3", "sqlite":
		c.DBDriver = "sqlite3"
		dataDir := kv["datadirectory"]
		if dataDir == "" {
			dataDir = filepath.Dir(path)
		}
		c.DBDSN = filepath.Join(dataDir, "nextcloud.db")
	case "oci":
		return fmt.Errorf("Oracle DB (oci) is not supported")
	default:
		return fmt.Errorf("unknown dbtype %q in config.php", dbtype)
	}

	if prefix := kv["dbtableprefix"]; prefix != "" {
		c.DBPrefix = prefix
	}
	if url := kv["overwrite.cli.url"]; url != "" {
		c.NextcloudURL = strings.TrimRight(url, "/")
	}

	return nil
}

func buildMySQLDSN(kv map[string]string) string {
	host := kv["dbhost"]
	if host == "" {
		host = "localhost"
	}

	cfg := mysql.NewConfig()
	cfg.User = kv["dbuser"]
	cfg.Passwd = kv["dbpassword"]
	cfg.DBName = kv["dbname"]
	cfg.ParseTime = true
	cfg.Params = map[string]string{"charset": "utf8mb4"}

	// Nextcloud allows dbhost to be "host", "host:port", "host:/path/to.sock"
	// or a bare socket path. Anything after the colon that looks like a path is
	// a unix socket, not a port.
	hostPart, portPart, hasColon := strings.Cut(host, ":")
	switch {
	case strings.HasPrefix(host, "/"):
		cfg.Net = "unix"
		cfg.Addr = host
	case hasColon && strings.HasPrefix(portPart, "/"):
		cfg.Net = "unix"
		cfg.Addr = portPart
	case hasColon && portPart != "":
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(hostPart, portPart)
	default:
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(host, "3306")
	}

	return cfg.FormatDSN()
}

func buildPostgresDSN(kv map[string]string) string {
	host := kv["dbhost"]
	if host == "" {
		host = "localhost"
	}
	port := "5432"
	if h, p, ok := strings.Cut(host, ":"); ok {
		host = h
		port = p
	}

	// A unix socket directory is given as the host; lib/pq takes it via the
	// `host` parameter, which must not be treated as a URL authority.
	if strings.HasPrefix(host, "/") {
		return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=prefer",
			quotePGValue(host), port, quotePGValue(kv["dbuser"]),
			quotePGValue(kv["dbpassword"]), quotePGValue(kv["dbname"]))
	}

	// URL form so that passwords containing spaces, quotes or '@' survive
	// intact — plain concatenation silently corrupts the connection string.
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(kv["dbuser"], kv["dbpassword"]),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + kv["dbname"],
		RawQuery: "sslmode=prefer",
	}
	return u.String()
}

// quotePGValue escapes a value for the key=value DSN form, where spaces and
// single quotes are significant.
func quotePGValue(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, ` '\`) {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// parseDatabaseURL turns a "driver://..." URL into (driverName, dsn).
func parseDatabaseURL(u string) (string, string, error) {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return "", "", fmt.Errorf("--database-url must be driver://..., got: %s", u)
	}
	switch strings.ToLower(scheme) {
	case "mysql", "mariadb":
		return "mysql", rest, nil
	case "postgres", "postgresql":
		return "postgres", u, nil // lib/pq accepts the full URL
	case "sqlite3", "sqlite":
		return "sqlite3", rest, nil
	default:
		return "", "", fmt.Errorf("unsupported database scheme %q", scheme)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}
