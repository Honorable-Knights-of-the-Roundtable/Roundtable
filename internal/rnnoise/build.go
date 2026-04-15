//go:build ignore

package main

import (
	"archive/tar"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	// MSYS2 UCRT64 rnnoise package - pre-built binaries (mingw64 variant no longer exists).
	// Check https://packages.msys2.org/packages/mingw-w64-ucrt-x86_64-rnnoise for the latest version.
	msys2RnnoiseURL = "https://mirror.msys2.org/mingw/ucrt64/mingw-w64-ucrt-x86_64-rnnoise-0.2-2-any.pkg.tar.zst"

	vendorDir = "deps/rnnoise"

	// System-wide install location on Windows (mirrors the opus pattern)
	systemInstallDir = "C:\\rnnoise"
)

func main() {
	if err := build(); err != nil {
		fatal("Build failed: %v", err)
	}
	fmt.Println("✓ Build successful!")
}

func build() error {
	switch runtime.GOOS {
	case "windows":
		return buildWindows()
	case "linux":
		return buildLinux()
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// --------------------------------------------------------------------------------
// Linux

func buildLinux() error {
	fmt.Println("Checking for rnnoise via pkg-config...")
	if hasPackage("rnnoise") {
		fmt.Println("  ✓ rnnoise found")
		return nil
	}
	return handleMissingLinux()
}

func hasPackage(pkg string) bool {
	return exec.Command("pkg-config", "--exists", pkg).Run() == nil
}

func handleMissingLinux() error {
	fmt.Println("\n❌ rnnoise not found!")
	distro := detectDistro()

	switch distro {
	case "debian", "ubuntu":
		fmt.Println("  sudo apt-get install librnnoise-dev")
	case "fedora", "rhel", "centos":
		fmt.Println("  sudo dnf install rnnoise-devel")
	case "arch":
		fmt.Println("  sudo pacman -S rnnoise")
	default:
		fmt.Println("  Please install the rnnoise development package for your distribution.")
	}

	fmt.Println("\nWould you like to install it now? (y/N)")
	if !askConfirmation() {
		return fmt.Errorf("rnnoise is required to build")
	}

	return installLinux(distro)
}

func installLinux(distro string) error {
	var cmd *exec.Cmd
	switch distro {
	case "debian", "ubuntu":
		cmd = exec.Command("sudo", "apt", "install", "-y", "librnnoise-dev")
	case "fedora", "rhel", "centos":
		cmd = exec.Command("sudo", "dnf", "install", "-y", "rnnoise-devel")
	case "arch":
		cmd = exec.Command("sudo", "pacman", "-S", "--noconfirm", "rnnoise")
	default:
		return fmt.Errorf("automatic installation not supported for your distribution")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installation failed: %w", err)
	}
	fmt.Println("\n✓ Installation successful!")
	return nil
}

// --------------------------------------------------------------------------------
// Windows

func buildWindows() error {
	systemLibPath := filepath.Join(systemInstallDir, "lib", "librnnoise.a")
	binPath := filepath.Join(systemInstallDir, "bin")

	needsInstall := !fileExists(systemLibPath)
	needsPathUpdate := needsInstall || !isInSystemPath(binPath)

	if needsPathUpdate && !isAdmin() {
		fmt.Println("Administrator privileges required for installation and PATH modification.")
		fmt.Println("Requesting elevation...")
		return rerunAsAdmin()
	}

	if fileExists(systemLibPath) {
		fmt.Printf("✓ librnnoise already installed at %s\n", systemInstallDir)
		if !isInSystemPath(binPath) {
			fmt.Println("Adding to system PATH...")
			if err := addToSystemPath(binPath); err != nil {
				fmt.Printf("⚠ Warning: Could not add to PATH: %v\n", err)
				fmt.Printf("Please manually add to PATH: %s\n", binPath)
			} else {
				fmt.Println("✓ Added to system PATH")
				fmt.Println("  (Restart your terminal/IDE for PATH changes to take effect)")
			}
		} else {
			fmt.Printf("✓ %s is already in PATH\n", binPath)
		}
		return nil
	}

	fmt.Println("Downloading rnnoise from MSYS2...")
	os.MkdirAll(vendorDir, 0755)
	tarPath := filepath.Join(vendorDir, "rnnoise.tar.zst")

	if err := downloadFile(msys2RnnoiseURL, tarPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	fmt.Println("Extracting...")
	extractDir := filepath.Join(vendorDir, "extracted")
	if err := extractTarZst(tarPath, extractDir); err != nil {
		return err
	}
	defer os.RemoveAll(extractDir)

	fmt.Printf("Installing to %s (requires admin privileges)...\n", systemInstallDir)
	msys2Root := filepath.Join(extractDir, "ucrt64")

	for _, sub := range []string{"lib", "include", "bin"} {
		src := filepath.Join(msys2Root, sub)
		dst := filepath.Join(systemInstallDir, sub)
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("failed to copy %s: %w", sub, err)
		}
	}

	fmt.Println("✓ Installation successful!")

	fmt.Println("Adding to system PATH...")
	if err := addToSystemPath(binPath); err != nil {
		fmt.Printf("⚠ Warning: Could not add to PATH automatically: %v\n", err)
		fmt.Printf("Please manually add to PATH: %s\n", binPath)
	} else {
		fmt.Println("✓ Added to system PATH")
		fmt.Println("\n⚠ IMPORTANT: Restart your terminal/shell for PATH changes to take effect!")
	}

	fmt.Printf("\nInstallation locations:\n")
	fmt.Printf("  Libraries: %s\n", filepath.Join(systemInstallDir, "lib"))
	fmt.Printf("  Headers:   %s\n", filepath.Join(systemInstallDir, "include"))
	fmt.Printf("  Binaries:  %s\n", binPath)

	return nil
}

// --------------------------------------------------------------------------------
// Helpers (mirrors opus/build.go)

func detectDistro() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	content := strings.ToLower(string(data))
	for _, name := range []string{"ubuntu", "debian", "fedora", "rhel", "centos", "arch"} {
		if strings.Contains(content, name) {
			return name
		}
	}
	return "unknown"
}

func askConfirmation() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	buf := make([]byte, 4)
	n, _ := tty.Read(buf)
	resp := strings.ToLower(strings.TrimSpace(string(buf[:n])))
	return resp == "y" || resp == "yes"
}

func downloadFile(url, path string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func extractTarZst(tarPath, dstDir string) error {
	file, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer file.Close()

	d, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer d.Close()

	tr := tar.NewReader(d)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			io.Copy(f, tr)
			f.Close()
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	os.MkdirAll(filepath.Dir(dst), 0755)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		return copyFile(path, dstPath)
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func addToSystemPath(dir string) error {
	psScript := fmt.Sprintf(`
		$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
		if ($path -notlike '*%s*') {
			[Environment]::SetEnvironmentVariable('Path', $path + ';%s', 'Machine')
			Write-Output 'Added to PATH'
		} else {
			Write-Output 'Already in PATH'
		}
	`, dir, dir)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}
	return nil
}

func isAdmin() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return exec.Command("net", "session").Run() == nil
}

func isInSystemPath(dir string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	psScript := fmt.Sprintf(`
		$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
		if ($path -like '*%s*') { Write-Output 'true' } else { Write-Output 'false' }
	`, dir)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}

func rerunAsAdmin() error {
	cwd, _ := os.Getwd()
	psScript := fmt.Sprintf(`Start-Process -FilePath "go" -ArgumentList "run","build.go" -Verb RunAs -WorkingDirectory "%s" -Wait`, cwd)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to elevate: %w (you may have cancelled the UAC prompt)", err)
	}
	fmt.Println("\n✓ Elevated process completed. You can now build your project.")
	os.Exit(0)
	return nil
}
