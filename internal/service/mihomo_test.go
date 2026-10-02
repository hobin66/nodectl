package service

import "testing"

func TestMihomoAssetFilename(t *testing.T) {
	tests := []struct {
		goos, goarch, version string
		wantName              string
		wantZip               bool
	}{
		{"linux", "amd64", "v1.20.0", "mihomo-linux-amd64-v1.20.0.gz", false},
		{"linux", "arm64", "v1.20.0", "mihomo-linux-arm64-v1.20.0.gz", false},
		{"windows", "amd64", "v1.20.0", "mihomo-windows-amd64-v1.20.0.zip", true},
		{"windows", "arm64", "v1.20.0", "mihomo-windows-arm64-v1.20.0.zip", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.goarch, func(t *testing.T) {
			name, isZip, err := mihomoAssetFilename(tt.goos, tt.goarch, tt.version)
			if err != nil {
				t.Fatal(err)
			}
			if name != tt.wantName || isZip != tt.wantZip {
				t.Fatalf("mihomoAssetFilename() = (%q, %t), want (%q, %t)", name, isZip, tt.wantName, tt.wantZip)
			}
		})
	}
}
