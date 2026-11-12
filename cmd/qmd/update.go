package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/indexer"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

var updateCmd = &cobra.Command{
	Use:           "update",
	Short:         "Re-index collections, in every index unless --index names one",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := updateTargets(cmd.Flag("index").Changed, getIndexName())
		if err != nil {
			return err
		}
		pull, _ := cmd.Flags().GetBool("pull")
		_, _ = cmd.Flags().GetBool("full") // accept for compatibility; full re-index is the default
		var errs []error
		for _, name := range names {
			if err := updateIndex(name, pull); err != nil {
				errs = append(errs, fmt.Errorf("index %q: %w", name, err))
			}
		}
		return errors.Join(errs...)
	},
}

// updateTargets answers which indexes this run touches. With --index it is that
// one index and a missing config is an error; without it, every <index>.yml in
// the config dir, so a new index needs no service edit.
func updateTargets(explicit bool, name string) ([]string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}
	reserved := config.ServerConfigName[:len(config.ServerConfigName)-len(".yml")]
	if explicit {
		if name == "" {
			return nil, fmt.Errorf("--index needs an index name")
		}
		if name == reserved {
			return nil, fmt.Errorf("%s holds mounts and keys, not collections; name an index from %s",
				filepath.Join(dir, config.ServerConfigName), dir)
		}
		path := filepath.Join(dir, name+".yml")
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("no index config at %s; set QMD_CONFIG_DIR to the directory holding <index>.yml", path)
		}
		return []string{name}, nil
	}
	names, err := config.IndexNames()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no <index>.yml in %s; set QMD_CONFIG_DIR to that directory, or create one with qmd --index <name> collection add .", dir)
	}
	return names, nil
}

func updateIndex(name string, pull bool) error {
	config.CurrentIndexName = name
	cfg, err := config.LoadConfigFor(name)
	if err != nil {
		return err
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	if len(cfg.Collections) == 0 {
		fmt.Printf("Index %q (%s): no collections\n", name, filepath.Join(dir, name+".yml"))
		return nil
	}
	dbPath, err := config.DBPathFor(name)
	if err != nil {
		return err
	}
	s, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	fmt.Printf("Index %q -> %s\n", name, dbPath)
	collections := make([]string, 0, len(cfg.Collections))
	for col := range cfg.Collections {
		collections = append(collections, col)
	}
	sort.Strings(collections)
	for _, colName := range collections {
		col := cfg.Collections[colName]
		if pull {
			if _, err := os.Stat(filepath.Join(col.Path, ".git")); err == nil {
				c := exec.Command("git", "pull")
				c.Dir = col.Path
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr
				if runErr := c.Run(); runErr != nil {
					fmt.Printf("Warning: git pull in %s failed: %v\n", col.Path, runErr)
				}
			}
		}
		fmt.Printf("Updating %s/%s...\n", name, colName)
		if err := indexer.IndexFiles(s, colName, col.Path, col.Pattern); err != nil {
			return fmt.Errorf("collection %q: %w", colName, err)
		}
	}
	return nil
}

func init() {
	updateCmd.Flags().Bool("pull", false, "Run git pull in each collection root before re-indexing")
	updateCmd.Flags().Bool("full", false, "Full re-index (default behavior; accepted for compatibility)")
	rootCmd.AddCommand(updateCmd)
}
