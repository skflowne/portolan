package pathnorm

import (
	"strings"
	"testing"
)

func TestIsWindowsPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"drive backslash", `C:\Users\me\proj\a.ts`, true},
		{"drive forward slash", `C:/Users/me/proj/a.ts`, true},
		{"lowercase drive", `c:\Users\me`, true},
		{"bare drive", `C:`, true},
		{"wsl dollar UNC", `\\wsl$\Ubuntu\home\me\a.ts`, true},
		{"wsl localhost UNC", `\\wsl.localhost\Ubuntu\home\me\a.ts`, true},
		{"generic UNC", `\\server\share\file.ts`, true},
		{"posix mount", `/mnt/c/Users/me/a.ts`, false},
		{"posix path", `/home/me/proj/a.ts`, false},
		{"relative path", `Users\me\proj\a.ts`, false},
		{"empty", ``, false},
		{"letter without colon", `C\Users`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWindowsPath(tc.path); got != tc.want {
				t.Errorf("IsWindowsPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		distro string
		want   string
	}{
		{"drive backslash", `C:\Users\me\proj\a.ts`, "Ubuntu", `/mnt/c/Users/me/proj/a.ts`},
		{"drive forward slash", `C:/Users/me/proj/a.ts`, "Ubuntu", `/mnt/c/Users/me/proj/a.ts`},
		{"lowercase drive", `d:\code\repo\file.go`, "Ubuntu", `/mnt/d/code/repo/file.go`},
		{"mixed separators", `C:\Users/me\proj/a.ts`, "Ubuntu", `/mnt/c/Users/me/proj/a.ts`},
		{"drive root", `C:\`, "Ubuntu", `/mnt/c`},
		{"drive traversal stays in mount", `C:\..\x`, "Ubuntu", `/mnt/c/x`},
		{"canonical mount", `/mnt/c/Users/me/a.ts`, "Ubuntu", `/mnt/c/Users/me/a.ts`},
		{"uppercase mount drive", `/mnt/C/Users/me/a.ts`, "Ubuntu", `/mnt/c/Users/me/a.ts`},
		{"wsl dollar UNC", `\\wsl$\Ubuntu\home\me\a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl dollar UNC case insensitive distro", `\\WSL$\uBuNtU\home\me\a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl localhost UNC", `\\wsl.localhost\Ubuntu\home\me\a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl localhost UNC case insensitive distro", `\\WSL.LOCALHOST\uBuNtU\home\me\a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl UNC root", `\\wsl$\Ubuntu`, "Ubuntu", `/`},
		{"wsl UNC mixed separators", `\\wsl$\Ubuntu/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"posix path", `/home/me/proj/a.ts`, "Ubuntu", `/home/me/proj/a.ts`},
		{"posix clean", `/home/me//proj/../proj/a.ts`, "Ubuntu", `/home/me/proj/a.ts`},
		{"posix backslash is a filename character", `/home/me/a\b.ts`, "Ubuntu", `/home/me/a\b.ts`},
		{"nonexistent path remains lexical", `/definitely/not/a/real/../path/a.ts`, "Ubuntu", `/definitely/not/a/path/a.ts`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tc.distro)
			got, err := Canonicalize(tc.input)
			if err != nil {
				t.Fatalf("Canonicalize(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("Canonicalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeRejectsInvalidPaths(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		distro string
	}{
		{"empty", ``, "Ubuntu"},
		{"relative forward slash", `home/me/a.ts`, "Ubuntu"},
		{"relative backslash", `Users\me\a.ts`, "Ubuntu"},
		{"bare drive", `C:`, "Ubuntu"},
		{"drive relative", `C:a.ts`, "Ubuntu"},
		{"unsupported UNC", `\\server\share\file.ts`, "Ubuntu"},
		{"unsupported slash network share", `//server/share/file.ts`, "Ubuntu"},
		{"UNC missing distro", `\\wsl$`, "Ubuntu"},
		{"UNC empty distro", `\\wsl$\\home\me\a.ts`, "Ubuntu"},
		{"UNC over prefixed", `\\\\wsl$\Ubuntu\home\me\a.ts`, "Ubuntu"},
		{"wsl dollar cross distro", `\\wsl$\Debian\home\me\a.ts`, "Ubuntu"},
		{"wsl localhost cross distro", `\\wsl.localhost\Debian\home\me\a.ts`, "Ubuntu"},
		{"wsl dollar distro unavailable", `\\wsl$\Ubuntu\home\me\a.ts`, ""},
		{"wsl localhost distro unavailable", `\\wsl.localhost\Ubuntu\home\me\a.ts`, ""},
		{"NUL", "/home/me/a\x00.ts", "Ubuntu"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tc.distro)
			got, err := Canonicalize(tc.input)
			if err == nil || got != "" {
				t.Fatalf("Canonicalize(%q) = %q, %v; want empty path and error", tc.input, got, err)
			}
		})
	}
}

func TestCanonicalizeIdempotent(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	inputs := []string{
		`C:\Users\me\proj\a.ts`,
		`C:/Users/me/proj/a.ts`,
		`\\wsl$\Ubuntu\home\me\a.ts`,
		`\\wsl.localhost\Ubuntu\home\me\a.ts`,
		`/home/me/proj/a.ts`,
		`/mnt/C/Users/me/a.ts`,
	}
	for _, input := range inputs {
		once, err := Canonicalize(input)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", input, err)
		}
		twice, err := Canonicalize(once)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", once, err)
		}
		if once != twice {
			t.Errorf("Canonicalize not idempotent for %q: first %q, second %q", input, once, twice)
		}
	}
}

func TestNormalizeCompatibility(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	cases := map[string]string{
		`/home/me//a.ts`:             `/home/me/a.ts`,
		`C:\Users\me\a.ts`:           `/mnt/c/Users/me/a.ts`,
		`\\wsl$\Ubuntu\home\me\a.ts`: `/home/me/a.ts`,
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWSLToWindows(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		distro  string
		want    string
		wantErr bool
	}{
		{"mount drive", `/mnt/c/Users/me/proj/a.ts`, "", `C:\Users\me\proj\a.ts`, false},
		{"mount drive root", `/mnt/c`, "", `C:\`, false},
		{"uppercase mount drive", `/mnt/D/code/repo/file.go`, "", `D:\code\repo\file.go`, false},
		{"non mount with distro", `/home/me/proj/a.ts`, "Ubuntu", `\\wsl.localhost\Ubuntu\home\me\proj\a.ts`, false},
		{"non mount without distro", `/home/me/proj/a.ts`, "", ``, true},
		{"Windows input", `c:/Users/me\a.ts`, "", `C:\Users\me\a.ts`, false},
		{"empty", ``, "", ``, true},
		{"relative", `home/me/a.ts`, "Ubuntu", ``, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tc.distro)
			got, err := WSLToWindows(tc.input)
			if tc.wantErr {
				if err == nil || got != "" {
					t.Fatalf("WSLToWindows(%q) = %q, %v; want empty path and error", tc.input, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WSLToWindows(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("WSLToWindows(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestPathToURI(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"mount path", `/mnt/c/Users/me/proj/a.ts`, `file:///mnt/c/Users/me/proj/a.ts`},
		{"posix path", `/home/me/a.ts`, `file:///home/me/a.ts`},
		{"space", `/home/me/my proj/a.ts`, `file:///home/me/my%20proj/a.ts`},
		{"Unicode", `/home/me/déjà/文件.ts`, `file:///home/me/d%C3%A9j%C3%A0/%E6%96%87%E4%BB%B6.ts`},
		{"percent", `/home/me/100%/a.ts`, `file:///home/me/100%25/a.ts`},
		{"fragment character", `/home/me/a#b.ts`, `file:///home/me/a%23b.ts`},
		{"query character", `/home/me/a?b.ts`, `file:///home/me/a%3Fb.ts`},
		{"Windows drive", `C:\Users\me\a.ts`, `file:///mnt/c/Users/me/a.ts`},
		{"wsl UNC", `\\wsl$\Ubuntu\home\me\a.ts`, `file:///home/me/a.ts`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
			got, err := PathToURI(tc.path)
			if err != nil {
				t.Fatalf("PathToURI(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("PathToURI(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestPathToURIRejectsInvalidPaths(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	for _, input := range []string{"", "relative/a.ts", `C:`, `\\server\share\a.ts`, "/home/me/a\x00.ts"} {
		t.Run(input, func(t *testing.T) {
			got, err := PathToURI(input)
			if err == nil || got != "" {
				t.Fatalf("PathToURI(%q) = %q, %v; want empty URI and error", input, got, err)
			}
		})
	}
}

func TestURIToPath(t *testing.T) {
	cases := []struct {
		name   string
		uri    string
		distro string
		want   string
	}{
		{"empty authority", `file:///mnt/c/Users/me/a.ts`, "Ubuntu", `/mnt/c/Users/me/a.ts`},
		{"space", `file:///home/me/my%20proj/a.ts`, "Ubuntu", `/home/me/my proj/a.ts`},
		{"Unicode", `file:///home/me/d%C3%A9j%C3%A0/%E6%96%87%E4%BB%B6.ts`, "Ubuntu", `/home/me/déjà/文件.ts`},
		{"localhost", `file://localhost/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"localhost case insensitive", `file://LOCALHOST/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"Windows drive", `file:///C:/Users/me/a.ts`, "Ubuntu", `/mnt/c/Users/me/a.ts`},
		{"localhost Windows drive", `file://localhost/C:/Users/me/a.ts`, "Ubuntu", `/mnt/c/Users/me/a.ts`},
		{"wsl dollar authority", `file://wsl$/Ubuntu/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl dollar authority case insensitive", `file://WSL$/uBuNtU/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl localhost authority", `file://wsl.localhost/Ubuntu/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
		{"wsl localhost authority case insensitive", `file://WSL.LOCALHOST/uBuNtU/home/me/a.ts`, "Ubuntu", `/home/me/a.ts`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tc.distro)
			got, err := URIToPath(tc.uri)
			if err != nil {
				t.Fatalf("URIToPath(%q): %v", tc.uri, err)
			}
			if got != tc.want {
				t.Errorf("URIToPath(%q) = %q, want %q", tc.uri, got, tc.want)
			}
		})
	}
}

func TestURIToPathRejectsInvalidURIs(t *testing.T) {
	cases := []struct {
		name   string
		uri    string
		distro string
	}{
		{"non-file scheme", `https://example.com/a.ts`, "Ubuntu"},
		{"malformed authority escape", `file://%zz`, "Ubuntu"},
		{"malformed path escape", `file:///home/me/%zz`, "Ubuntu"},
		{"empty URI path", `file://`, "Ubuntu"},
		{"empty localhost path", `file://localhost`, "Ubuntu"},
		{"empty file URI", `file:`, "Ubuntu"},
		{"opaque relative path", `file:relative/a.ts`, "Ubuntu"},
		{"unsupported authority", `file://server/share/a.ts`, "Ubuntu"},
		{"authority userinfo", `file://user@localhost/home/me/a.ts`, "Ubuntu"},
		{"authority port", `file://localhost:80/home/me/a.ts`, "Ubuntu"},
		{"query", `file:///home/me/a.ts?query`, "Ubuntu"},
		{"fragment", `file:///home/me/a.ts#fragment`, "Ubuntu"},
		{"decoded NUL", `file:///home/me/%00a.ts`, "Ubuntu"},
		{"wsl dollar missing distro", `file://wsl$/`, "Ubuntu"},
		{"wsl localhost missing distro", `file://wsl.localhost/`, "Ubuntu"},
		{"wsl dollar cross distro", `file://wsl$/Debian/home/me/a.ts`, "Ubuntu"},
		{"wsl localhost cross distro", `file://wsl.localhost/Debian/home/me/a.ts`, "Ubuntu"},
		{"wsl dollar distro unavailable", `file://wsl$/Ubuntu/home/me/a.ts`, ""},
		{"wsl localhost distro unavailable", `file://wsl.localhost/Ubuntu/home/me/a.ts`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tc.distro)
			got, err := URIToPath(tc.uri)
			if err == nil || got != "" {
				t.Fatalf("URIToPath(%q) = %q, %v; want empty path and error", tc.uri, got, err)
			}
		})
	}
}

func TestPathURIRoundTrip(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	cases := []struct {
		input string
		want  string
	}{
		{`/home/me/my proj/a.ts`, `/home/me/my proj/a.ts`},
		{`/mnt/C/Users/me/déjà vu/文件.ts`, `/mnt/c/Users/me/déjà vu/文件.ts`},
		{`C:\Users\me\a.ts`, `/mnt/c/Users/me/a.ts`},
		{`\\wsl.localhost\Ubuntu\home\me\a.ts`, `/home/me/a.ts`},
	}
	for _, tc := range cases {
		uri, err := PathToURI(tc.input)
		if err != nil {
			t.Fatalf("PathToURI(%q): %v", tc.input, err)
		}
		got, err := URIToPath(uri)
		if err != nil {
			t.Fatalf("URIToPath(%q): %v", uri, err)
		}
		if got != tc.want {
			t.Errorf("round trip %q -> %q -> %q, want %q", tc.input, uri, got, tc.want)
		}
	}
}

func TestURIPathRoundTrip(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	cases := []struct {
		uri  string
		want string
	}{
		{`file:///home/me/a.ts`, `/home/me/a.ts`},
		{`file://localhost/home/me/my%20proj/a.ts`, `/home/me/my proj/a.ts`},
		{`file:///C:/Users/me/a.ts`, `/mnt/c/Users/me/a.ts`},
		{`file://wsl$/Ubuntu/home/me/a.ts`, `/home/me/a.ts`},
		{`file://wsl.localhost/Ubuntu/home/me/a.ts`, `/home/me/a.ts`},
	}
	for _, tc := range cases {
		canonical, err := URIToPath(tc.uri)
		if err != nil {
			t.Fatalf("URIToPath(%q): %v", tc.uri, err)
		}
		uri, err := PathToURI(canonical)
		if err != nil {
			t.Fatalf("PathToURI(%q): %v", canonical, err)
		}
		got, err := URIToPath(uri)
		if err != nil {
			t.Fatalf("URIToPath(%q): %v", uri, err)
		}
		if canonical != tc.want || got != tc.want {
			t.Errorf("round trip %q -> %q -> %q -> %q, want canonical path %q", tc.uri, canonical, uri, got, tc.want)
		}
	}
}

func TestWSLWindowsRoundTrip(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	for _, input := range []string{`/mnt/c/Users/me/a.ts`, `/mnt/d/code/repo/file.go`, `/home/me/a.ts`} {
		windows, err := WSLToWindows(input)
		if err != nil {
			t.Fatalf("WSLToWindows(%q): %v", input, err)
		}
		got, err := Canonicalize(windows)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", windows, err)
		}
		if got != input {
			t.Errorf("round trip %q -> %q -> %q", input, windows, got)
		}
	}
}

func TestWindowsDriveRoundTripIgnoresDriveCase(t *testing.T) {
	for _, input := range []string{`C:\Users\me\a.ts`, `d:\code\repo\file.go`} {
		wsl, err := Canonicalize(input)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", input, err)
		}
		got, err := WSLToWindows(wsl)
		if err != nil {
			t.Fatalf("WSLToWindows(%q): %v", wsl, err)
		}
		if !strings.EqualFold(got, input) {
			t.Errorf("round trip %q -> %q -> %q", input, wsl, got)
		}
	}
}
