package config

type Collection struct {
	Path    string            `yaml:"path"`
	Pattern string            `yaml:"pattern"`
	Context map[string]string `yaml:"context,omitempty"`
	Update  string            `yaml:"update,omitempty"`
}

type Config struct {
	// DBPath optionally pins this index's SQLite file (e.g. /mnt/srv/.qmd/index.sqlite).
	// Empty = resolve via QMD_DB_DIR, INDEX_PATH or the XDG cache dir (see config.DBPathFor).
	DBPath        string                `yaml:"db_path,omitempty"`
	GlobalContext string                `yaml:"global_context,omitempty"`
	Collections   map[string]Collection `yaml:"collections"`
}
