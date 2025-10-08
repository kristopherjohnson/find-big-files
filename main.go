// Package main provides a utility to find large files on disk with support for
// sparse file detection, deletable file categorization, and concurrent processing.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/trace"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FileInfo holds information about a file or directory including size,
// modification time, and categorization metadata.
type FileInfo struct {
	Path        string
	Size        int64
	ActualSize  int64 // Actual disk usage (for sparse files)
	ModTime     time.Time
	IsDeletable bool
	Category    string
	IsSparse    bool
	IsDirectory bool // True if this represents a directory aggregate
}

// Common patterns for likely deletable files
var deletablePatterns = []string{
	"*.log", "*.log.*", "*.log[0-9]*",
	"*.tmp", "*.temp", "*.cache", "*.bak", "*.backup",
	"*.o", "*.a", "*.so", "*.dylib",
	".DS_Store", "Thumbs.db",
	"*.dmp", "*.dump", "core", "core.*",
	"*.swp", "*.swo", "*~",
}

// Directories that often contain deletable content
var deletableDirs = []string{
	"node_modules", "target", "build", "dist", "out",
	".cache", ".npm", ".yarn", ".gradle",
	"Pods", ".direnv", "__pycache__",
	"tmp", "temp", ".temp",
}

// Directories to skip entirely
var skipDirs = []string{
	".git", ".svn", ".hg",
}

func main() {
	defer trace.StartRegion(context.Background(), "app/main").End()

	// Command-line flags
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Warning: could not get home directory: %v", err)
		homeDir = "." // fallback to current directory
	}

	searchDir := flag.String("dir", homeDir, "Directory to search")
	minSize := flag.Int64("min-size", 1<<30, "Minimum file size in bytes (default 1GB)")
	topN := flag.Int("top", 20, "Number of files to display")
	showAll := flag.Bool("all", false, "Show all large files regardless of type")
	showDeletable := flag.Bool("deletable", true, "Highlight potentially deletable files")
	tracePath := flag.String("trace", "", "Enable tracing and write trace output to the specified file")

	flag.Parse()

	// Setup tracing if requested
	if *tracePath != "" {
		f, err := os.Create(*tracePath)
		if err != nil {
			log.Fatalf("failed to create trace output file: %v", err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Fatalf("failed to close trace file: %v", err)
			}
		}()

		if err := trace.Start(f); err != nil {
			log.Fatalf("failed to start trace: %v", err)
		}
		defer trace.Stop()
		fmt.Printf("Tracing enabled, output will be written to: %s\n", *tracePath)
	}

	fmt.Printf("Searching for files larger than %s in %s...\n", formatSize(*minSize), *searchDir)
	if !*showAll {
		fmt.Println("(Focusing on potentially deletable files)")
	}
	fmt.Println()

	// Create context with timeout for the operation
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	files, err := findLargeFiles(ctx, *searchDir, *minSize)
	if err != nil {
		log.Fatalf("Error searching for files: %v", err)
	}

	// Sort by actual disk usage (largest first)
	// For sparse files, this shows true disk impact; for regular files, it's the same as file size
	reg := trace.StartRegion(ctx, "app/main/sort")
	sort.Slice(files, func(i, j int) bool {
		return files[i].ActualSize > files[j].ActualSize
	})
	reg.End()

	// Limit to top N
	if len(files) > *topN {
		files = files[:*topN]
	}

	// Display results
	if len(files) == 0 {
		fmt.Printf("No files found larger than %s\n", formatSize(*minSize))
		return
	}

	fmt.Printf("Top %d largest files:\n", len(files))
	fmt.Println(strings.Repeat("-", 132))

	var totalSize int64
	var deletableSize int64

	for i, file := range files {
		marker := ""
		if *showDeletable && file.IsDeletable {
			marker = " [LIKELY DELETABLE]"
			deletableSize += file.ActualSize
		}

		relPath := file.Path
		if after, found := strings.CutPrefix(file.Path, homeDir); found {
			relPath = "~" + after
		}

		// Show actual disk usage for sparse files, apparent size otherwise
		displaySize := file.Size
		sizeInfo := formatSize(displaySize)

		if file.IsDirectory {
			marker += " [DIR]"
			if file.Size != file.ActualSize {
				sizeInfo = fmt.Sprintf("%s (actual: %s)", formatSize(file.Size), formatSize(file.ActualSize))
			}
		} else if file.IsSparse {
			marker += " [SPARSE]"
			sizeInfo = fmt.Sprintf("%s (actual: %s)", formatSize(file.Size), formatSize(file.ActualSize))
		}

		fmt.Printf("%2d. %-80s %20s%s\n",
			i+1,
			truncatePath(relPath, 80),
			sizeInfo,
			marker)

		if file.Category != "" {
			fmt.Printf("    Category: %s | Modified: %s\n",
				file.Category,
				file.ModTime.Format("2006-01-02 15:04"))
		}

		// Use actual size for total calculations
		totalSize += file.ActualSize
	}

	fmt.Println(strings.Repeat("-", 132))
	fmt.Printf("Total size: %s\n", formatSize(totalSize))
	if *showDeletable && deletableSize > 0 {
		fmt.Printf("Potentially deletable: %s (%.1f%%)\n",
			formatSize(deletableSize),
			float64(deletableSize)*100/float64(totalSize))
	}
}

func findLargeFiles(ctx context.Context, root string, minSize int64) ([]FileInfo, error) {
	defer trace.StartRegion(ctx, "app/findLargeFiles").End()

	// Use number of CPUs for worker pool size, but cap at reasonable limit
	numWorkers := min(runtime.NumCPU(), 8)

	// Channel for results - use buffered channel but not too large
	resultChan := make(chan FileInfo, numWorkers*10)

	// Use a WaitGroup to track all goroutines
	var wg sync.WaitGroup

	// Collect results
	var files []FileInfo
	var mu sync.Mutex

	// Start result collector in a separate goroutine
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer trace.StartRegion(ctx, "app/findLargeFiles/append").End()
		defer collectorWg.Done()
		for {
			select {
			case file, ok := <-resultChan:
				if !ok {
					return // Channel closed
				}
				mu.Lock()
				files = append(files, file)
				mu.Unlock()
			case <-ctx.Done():
				return // Context cancelled
			}
		}
	}()

	// Process root directory and spawn workers for subdirectories
	wg.Add(1)
	go func() {
		defer wg.Done()
		processDirectoryConcurrently(ctx, root, minSize, resultChan, &wg, numWorkers)
	}()

	// Wait for all processing to complete, then close result channel
	go func() {
		defer trace.StartRegion(ctx, "app/findLargeFiles/wg.Wait").End()
		wg.Wait()
		close(resultChan)
	}()

	// Wait for collector to finish
	reg := trace.StartRegion(ctx, "app/findLargeFiles/collectorWg.Wait")
	collectorWg.Wait()
	reg.End()

	// Check if context was cancelled
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return files, nil
}

// processDirectoryConcurrently processes a directory and its subdirectories concurrently
func processDirectoryConcurrently(ctx context.Context, dirPath string, minSize int64, resultChan chan<- FileInfo, wg *sync.WaitGroup, maxWorkers int) {
	defer trace.StartRegion(ctx, "app/processDirectoryConcurrently").End()
	defer trace.Logf(ctx, "app", "processDirectoryConcurrently %s", dirPath)

	// Check if context is cancelled
	if ctx.Err() != nil {
		return
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		// Silently skip directories we can't read (e.g., permission denied)
		return
	}

	// Semaphore to limit concurrent goroutines
	sem := make(chan struct{}, maxWorkers)

	var localWg sync.WaitGroup

	for _, entry := range entries {
		// Check context cancellation in the loop
		select {
		case <-ctx.Done():
			return
		default:
		}

		path := filepath.Join(dirPath, entry.Name())

		if entry.IsDir() {
			// Check if we should skip this directory
			dirName := entry.Name()
			shouldSkip := slices.Contains(skipDirs, dirName)

			if !shouldSkip {
				// Check if this is a deletable directory that should be aggregated
				if isDeletableDir(dirName) {
					// Process deletable directory as a single aggregate entry
					localWg.Add(1)
					wg.Add(1)
					go func(dirPath string, dirName string) {
						trace.StartRegion(ctx, "app/processDirectoryConcurrently/deletable").End()
						defer func() {
							localWg.Done()
							wg.Done()
							<-sem // Release semaphore
						}()
						sem <- struct{}{} // Acquire semaphore

						// Calculate total directory size
						totalSize, totalActualSize, newestModTime, err := calculateDirectorySize(ctx, dirPath)
						if err != nil {
							// Silently skip directories we can't process
							return
						}

						// Only report if it meets the size threshold
						if totalActualSize >= minSize {
							dirInfo := FileInfo{
								Path:        dirPath,
								Size:        totalSize,
								ActualSize:  totalActualSize,
								ModTime:     newestModTime,
								IsDeletable: true,
								Category:    fmt.Sprintf("%s directory", dirName),
								IsDirectory: true,
							}

							select {
							case resultChan <- dirInfo:
							case <-ctx.Done():
								return
							}
						}
					}(path, dirName)
				} else {
					// Process subdirectory concurrently (normal traversal)
					localWg.Add(1)
					wg.Add(1)
					go func(subPath string) {
						trace.StartRegion(ctx, "app/processDirectoryConcurrently/normal").End()
						defer func() {
							localWg.Done()
							wg.Done()
							<-sem // Release semaphore
						}()
						sem <- struct{}{} // Acquire semaphore
						processDirectoryConcurrently(ctx, subPath, minSize, resultChan, wg, maxWorkers)
					}(path)
				}
			}
		} else {
			// Process file
			if fileInfo := processFile(ctx, path, entry, minSize); fileInfo != nil {
				select {
				case resultChan <- *fileInfo:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	// Wait for all subdirectories in this level to complete
	localWg.Wait()
}

// processFile processes a single file and returns FileInfo if it meets criteria
func processFile(ctx context.Context, path string, entry fs.DirEntry, minSize int64) *FileInfo {
	defer trace.StartRegion(ctx, "app/processFile").End()
	trace.Logf(ctx, "app", "processFile: %s", path)

	info, err := entry.Info()
	if err != nil {
		return nil
	}

	// Check size threshold
	if info.Size() < minSize {
		return nil
	}

	// Get actual disk usage
	actualSize, err := getActualDiskUsage(path)
	if err != nil {
		// If we can't get actual disk usage, fall back to apparent size
		actualSize = info.Size()
	}

	// Determine if the file is sparse
	sparse := isSparseFile(info.Size(), actualSize) || isKnownSparseFile(path)

	// Create FileInfo and check if it's deletable
	fileInfo := FileInfo{
		Path:       path,
		Size:       info.Size(),
		ActualSize: actualSize,
		ModTime:    info.ModTime(),
		IsSparse:   sparse,
	}

	// Check if file matches deletable patterns
	fileName := filepath.Base(path)
	for _, pattern := range deletablePatterns {
		if matched, err := filepath.Match(pattern, fileName); err == nil && matched {
			fileInfo.IsDeletable = true
			fileInfo.Category = "Temporary/Cache"
			break
		}
		// Silently skip invalid patterns (unlikely with our hardcoded patterns)
	}

	// Check if file is in a deletable directory
	if !fileInfo.IsDeletable {
		for _, dir := range deletableDirs {
			if strings.Contains(path, "/"+dir+"/") {
				fileInfo.IsDeletable = true
				fileInfo.Category = fmt.Sprintf("%s directory", dir)
				break
			}
		}
	}

	// Check for specific file types
	if !fileInfo.IsDeletable {
		ext := strings.ToLower(filepath.Ext(fileName))
		switch ext {
		case ".dmg", ".iso", ".pkg", ".zip", ".tar", ".gz", ".bz2", ".xz":
			fileInfo.Category = "Archive/Installer"
			fileInfo.IsDeletable = true
		case ".mp4", ".mov", ".avi", ".mkv":
			fileInfo.Category = "Video"
		case ".log":
			fileInfo.Category = "Log file"
			fileInfo.IsDeletable = true
		}
	}

	// Check for macOS specific cache locations
	if strings.Contains(path, "/Library/Caches/") {
		fileInfo.IsDeletable = true
		fileInfo.Category = "System Cache"
	}

	// Check for Downloads folder (old files)
	if strings.Contains(path, "/Downloads/") {
		age := time.Since(info.ModTime())
		if age > 30*24*time.Hour { // Older than 30 days
			fileInfo.Category = "Old Download"
			fileInfo.IsDeletable = true
		}
	}

	// Special handling for sparse files
	if fileInfo.IsSparse {
		if fileInfo.Category == "" {
			if isKnownSparseFile(path) {
				fileInfo.Category = "Virtual Disk/Container"
			} else {
				fileInfo.Category = "Sparse File"
			}
		}
	}

	return &fileInfo
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}

	// Try to keep the filename and truncate the directory path
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		filename := parts[len(parts)-1]
		// Keep more of the path with the wider display
		if len(filename) < maxLen-15 {
			// Show beginning and end of path
			prefixLen := (maxLen - len(filename) - 5) / 2 // Reserve 5 for "..."
			suffixStart := len(path) - (maxLen - prefixLen - 3)
			if suffixStart > prefixLen {
				return path[:prefixLen] + "..." + path[suffixStart:]
			}
		}
	}

	// Fallback: just truncate from the end
	return path[:maxLen-3] + "..."
}

// getActualDiskUsage returns the actual disk usage for a file using syscall.Stat.
// On macOS and Linux, this calculates actual size = blocks * 512 bytes.
// This helps detect sparse files where apparent size differs from actual disk usage.
func getActualDiskUsage(path string) (int64, error) {
	var stat syscall.Stat_t
	err := syscall.Stat(path, &stat)
	if err != nil {
		return 0, err
	}

	// On macOS and Linux, stat.Blocks is in 512-byte units
	return stat.Blocks * 512, nil
}

// isSparseFile determines if a file is sparse by comparing apparent size vs actual disk usage
func isSparseFile(apparentSize, actualSize int64) bool {
	// Consider a file sparse if actual disk usage is significantly less than apparent size
	// We use a threshold to account for file system overhead and small differences
	const sparseThreshold = 0.95 // File is sparse if actual usage < 95% of apparent size

	if apparentSize == 0 {
		return false
	}

	ratio := float64(actualSize) / float64(apparentSize)
	return ratio < sparseThreshold
}

// isKnownSparseFile checks if a file is a known type that's commonly sparse
func isKnownSparseFile(path string) bool {
	knownSparseFiles := []string{
		"Docker.raw",
		".vmdk",
		".vdi",
		".qcow2",
		".img",
	}

	fileName := filepath.Base(path)
	for _, pattern := range knownSparseFiles {
		if strings.Contains(fileName, pattern) {
			return true
		}
	}

	return false
}

// calculateDirectorySize recursively calculates the total size of all files in a directory
func calculateDirectorySize(ctx context.Context, dirPath string) (int64, int64, time.Time, error) {
	defer trace.StartRegion(ctx, "app/calculateDirectorySize").End()
	trace.Logf(ctx, "app", "calculateDirectorySize %s", dirPath)

	var totalSize int64
	var totalActualSize int64
	var newestModTime time.Time

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		trace.StartRegion(ctx, "app/calculateDirectorySize/WalkDir").End()
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			// Skip files we can't access
			return nil
		}

		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return nil // Skip files we can't stat
			}

			totalSize += info.Size()

			// Get actual disk usage
			actualSize, err := getActualDiskUsage(path)
			if err != nil {
				actualSize = info.Size()
			}
			totalActualSize += actualSize

			// Track newest modification time
			if info.ModTime().After(newestModTime) {
				newestModTime = info.ModTime()
			}
		}
		return nil
	})

	return totalSize, totalActualSize, newestModTime, err
}

// isDeletableDir checks if a directory name matches any of the deletable directory patterns
func isDeletableDir(dirName string) bool {
	return slices.Contains(deletableDirs, dirName)
}
