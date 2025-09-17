# Find Big Files

A fast Go command-line tool to find large files on your system, with special attention to potentially deletable files like logs, caches, and temporary files. Features concurrent directory traversal for improved performance and accurate sparse file detection.

## Features

- **Fast parallel scanning** - Uses concurrent goroutines to traverse directories quickly
- **Sparse file detection** - Correctly reports actual disk usage for Docker images and VM files
- **Directory aggregation** - Shows total size for deletable directories like `node_modules`, `target`, `build`
- **Smart categorization** - Identifies potentially deletable files automatically
- **Flexible filtering** - Customizable size thresholds and result limits
- **Wide terminal support** - Optimized for 132+ character wide terminals

## Building

```bash
go build -o find-big-files
```

## Usage

```bash
# Find files larger than 1GB in your home directory (default)
./find-big-files

# Search a specific directory
./find-big-files -dir /path/to/directory

# Find files larger than 500MB
./find-big-files -min-size 524288000

# Show top 50 files instead of default 20
./find-big-files -top 50

# Show all large files regardless of type
./find-big-files -all

# Don't highlight deletable files
./find-big-files -deletable=false
```

## Command-line Options

- `-dir`: Directory to search (default: home directory)
- `-min-size`: Minimum file size in bytes (default: 1GB = 1073741824 bytes)
- `-top`: Number of files to display (default: 20)
- `-all`: Show all large files regardless of type
- `-deletable`: Highlight potentially deletable files (default: true)

## What It Identifies as "Deletable"

The program marks files and directories as potentially deletable based on:

1. **File extensions**: `.log`, `.tmp`, `.temp`, `.cache`, `.bak`, `.backup`, etc.
2. **Build artifacts**: `.o`, `.a`, `.so`, `.dylib`
3. **System files**: `.DS_Store`, `Thumbs.db`
4. **Aggregated directories**: Entire directories like `node_modules`, `target`, `build`, `dist`, `out`, `.cache`, `.npm`, `.yarn`, `.gradle`, `Pods`, `.direnv`, `__pycache__`, `tmp`, `temp`, `.temp`
5. **macOS caches**: Files in `*/Library/Caches/*`
6. **Old downloads**: Files in Downloads folder older than 30 days
7. **Archives**: `.dmg`, `.iso`, `.pkg`, `.zip`, `.tar`, etc.

## Sparse File Handling

The tool correctly detects and reports sparse files (like Docker.raw, .vmdk, .vdi files) that appear large but use less actual disk space. For example:

```
~/Library/Containers/com.docker.docker/Data/vms/0/Docker.raw   1.0 TB (actual: 91.6 GB) [SPARSE]
```

This shows the file appears as 1TB but only uses 91.6GB of actual disk space.

## Directory Aggregation

When the tool encounters directories that commonly contain deletable content (like `node_modules`, `target`, `build`), it aggregates the total size of all files within that directory and reports it as a single entry. This provides a cleaner output and makes it easier to identify which directories can be deleted to reclaim space.

## Example Output

```
Searching for files larger than 1.0 GB in /Users/username...
(Focusing on potentially deletable files)

Top 10 largest files:
------------------------------------------------------------------------------------------------------------------------------------
 1. ~/work/my-rust-app/target                                         10.8 GB (actual: 11.0 GB) [LIKELY DELETABLE] [DIRECTORY]
    Category: target directory | Modified: 2025-09-16 16:22
 2. ~/Library/Containers/com.docker.docker/Data/vm.../Docker.raw     1.0 TB (actual: 91.6 GB) [SPARSE]
    Category: Virtual Disk/Container | Modified: 2025-09-16 20:51
 3. ~/projects/webapp/node_modules                                               2.3 GB [LIKELY DELETABLE] [DIRECTORY]
    Category: node_modules directory | Modified: 2024-01-15 14:23
 4. ~/Downloads/macos-installer.dmg                                              1.8 GB [LIKELY DELETABLE]
    Category: Archive/Installer | Modified: 2023-12-01 09:15
 5. ~/sdks/android/my-app/build                                   1.0 GB (actual: 1.0 GB) [LIKELY DELETABLE] [DIRECTORY]
    Category: build directory | Modified: 2025-09-15 17:28
...
------------------------------------------------------------------------------------------------------------------------------------
Total size: 97.2 GB
Potentially deletable: 15.9 GB (16.4%)
```

## Performance

The tool uses concurrent goroutines to traverse directories in parallel, significantly speeding up scans of large directory trees. It automatically adjusts to use the optimal number of workers based on your system's CPU cores.

## Safety Note

This tool only **lists** files - it does not delete anything. Review the results carefully before manually deleting any files. Be especially cautious with:
- System files and caches that may be recreated automatically
- Virtual machine and container disk images
- Archives that may contain important data

