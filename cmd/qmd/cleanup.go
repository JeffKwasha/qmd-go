package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove cache and orphaned data, vacuum DB",
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			return
		}
		defer s.Close()

		res, _ := s.DB.Exec(`DELETE FROM llm_cache`)
		n, _ := res.RowsAffected()
		fmt.Printf("Cleared %d cached LLM response(s)\n", n)

		orphan, err := s.CleanupOrphanedContent()
		if err != nil {
			fmt.Printf("Error cleaning orphaned content: %v\n", err)
		} else if orphan > 0 {
			fmt.Printf("Removed %d orphaned content hash(es)\n", orphan)
		}

		_, _ = s.DB.Exec(`DELETE FROM content_vectors WHERE hash NOT IN (SELECT hash FROM documents WHERE active = 1)`)
		_, _ = s.DB.Exec(`DELETE FROM embedding_blobs WHERE hash_seq NOT IN (
			SELECT hash || '_' || seq FROM content_vectors
		)`)
		fmt.Println("Cleaned orphaned vectors")

		_, err = s.DB.Exec(`VACUUM`)
		if err != nil {
			fmt.Printf("Vacuum failed: %v\n", err)
		} else {
			fmt.Println("Database vacuumed")
		}
	},
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
}
