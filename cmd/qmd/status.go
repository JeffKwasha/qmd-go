package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show index status and collections",
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()

		st, err := s.GetStatus()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting status: %v\n", err)
			os.Exit(1)
		}

		var size int64
		if fi, err := os.Stat(st.DBPath); err == nil {
			size = fi.Size()
		}
		sizeStr := formatBytes(size)

		fmt.Println("QMD Status")
		fmt.Println()
		fmt.Println("Index:", st.DBPath)
		fmt.Println("Size:", sizeStr)
		fmt.Println()
		fmt.Println("Documents")
		fmt.Printf("  Total:    %d files indexed\n", st.DocCount)
		fmt.Printf("  Vectors:  %d embedded\n\n", st.VectorCount)

		cfg, _ := config.LoadConfig()
		fmt.Println("Collections")
		if len(cfg.Collections) == 0 && len(st.Collections) == 0 {
			fmt.Println("  No collections. Run 'qmd collection add .' to index files.")
			return
		}
		byName := make(map[string]int)
		for _, c := range st.Collections {
			byName[c.Name] = c.ActiveCount
		}
		for name, col := range cfg.Collections {
			cnt := byName[name]
			lastMod := ""
			for _, c := range st.Collections {
				if c.Name == name && c.LastModified != "" {
					lastMod = formatTimeAgo(c.LastModified)
					break
				}
			}
			fmt.Printf("  %s (qmd://%s/)\n", name, name)
			fmt.Printf("    Pattern: %s\n", col.Pattern)
			fmt.Printf("    Files:   %d", cnt)
			if lastMod != "" {
				fmt.Printf(" (updated %s)", lastMod)
			}
			fmt.Println()
		}
	},
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	if n < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
}

func formatTimeAgo(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
