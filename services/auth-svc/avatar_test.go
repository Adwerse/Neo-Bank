package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodeTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: 0, B: uint8(y % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// TestDecodeAvatarImage_ValidFormats is the DoD's success case: a real
// JPEG (and, since the allowlist covers it too, a real PNG) must decode
// cleanly.
func TestDecodeAvatarImage_ValidFormats(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"jpeg", encodeTestJPEG(t, 200, 150)},
		{"png", encodeTestPNG(t, 200, 150)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img, err := decodeAvatarImage(tt.data)
			if err != nil {
				t.Fatalf("decodeAvatarImage: unexpected error: %v", err)
			}
			if img.Bounds().Dx() != 200 || img.Bounds().Dy() != 150 {
				t.Errorf("decoded bounds = %v, want 200x150", img.Bounds())
			}
		})
	}
}

// TestDecodeAvatarImage_NotActuallyAnImage is the DoD's explicit
// requirement: a file with a plausible name/extension but that isn't
// really an image must be rejected. decodeAvatarImage never sees a file
// name or client-declared Content-Type at all — it only ever sees bytes —
// so this is really testing that non-image bytes fail the magic-byte
// sniff regardless of what anyone might have called the file.
func TestDecodeAvatarImage_NotActuallyAnImage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"plain text", []byte("this is definitely not an image, just text pretending")},
		{"zip magic bytes", []byte("PK\x03\x04" + "some zip-shaped garbage padding out the sniff window.....")},
		{"empty", []byte{}},
		{"truncated jpeg header only", []byte{0xFF, 0xD8, 0xFF}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeAvatarImage(tt.data); err == nil {
				t.Error("decodeAvatarImage = nil error, want rejection")
			}
		})
	}
}

// TestDecodeAvatarImage_UnsupportedFormat confirms the allowlist is
// closed: GIF is a real, valid image format that http.DetectContentType
// correctly identifies, but it's not in allowedAvatarContentTypes, so it
// must still be rejected.
func TestDecodeAvatarImage_UnsupportedFormat(t *testing.T) {
	// Minimal valid GIF89a header + trailer — enough for
	// http.DetectContentType to correctly sniff "image/gif".
	gif := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff\x21\xf9\x04\x00\x00\x00\x00\x00\x2c\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02\x44\x01\x00\x3b")
	if _, err := decodeAvatarImage(gif); err == nil {
		t.Error("decodeAvatarImage(gif) = nil error, want rejection (not in the explicit allowlist)")
	}
}

// TestDecodeAvatarImage_TooManyBytes pins the raw-size ceiling,
// independent of the presigned POST policy's own content-length-range —
// defense in depth, checked here regardless of what got the bytes into
// storage in the first place.
func TestDecodeAvatarImage_TooManyBytes(t *testing.T) {
	oversized := make([]byte, maxAvatarUploadBytes+1)
	if _, err := decodeAvatarImage(oversized); err == nil {
		t.Error("decodeAvatarImage(oversized) = nil error, want rejection")
	}
}

// TestDecodeAvatarImage_DecompressionBomb is the DoD's other explicit
// requirement: a small file that decodes to an enormous pixel grid must
// be rejected — and rejected because of the DECLARED DIMENSIONS
// (DecodeConfig, a header-only read), not because of raw byte size. A
// solid-color PNG compresses extremely well, so this fixture is
// simultaneously "small in bytes" and "huge in pixels" — the exact shape
// of a real decompression bomb.
func TestDecodeAvatarImage_DecompressionBomb(t *testing.T) {
	const side = 6000 // 36,000,000 pixels, over maxAvatarPixels (20,000,000)
	img := image.NewUniform(color.Gray{Y: 128})
	bounds := image.Rect(0, 0, side, side)
	canvas := image.NewPaletted(bounds, []color.Color{color.Gray{Y: 128}})
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			canvas.Set(x, y, img.At(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode bomb png: %v", err)
	}
	data := buf.Bytes()

	if len(data) > maxAvatarUploadBytes {
		t.Fatalf("test fixture itself is %d bytes, want well under maxAvatarUploadBytes (%d) to prove this is a resolution rejection, not a size rejection", len(data), maxAvatarUploadBytes)
	}

	_, err := decodeAvatarImage(data)
	if err == nil {
		t.Fatal("decodeAvatarImage(bomb) = nil error, want rejection")
	}
	t.Logf("bomb fixture: %d bytes for a %dx%d image, rejected as: %v", len(data), side, side, err)
}

func TestAvatarKeyOwnedBy(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		userID string
		want   bool
	}{
		{"exact own key", "avatars/user-1/upload-1", "user-1", true},
		{"different user's key", "avatars/user-2/upload-1", "user-1", false},
		{"already-processed thumbnail key", "avatars/user-1/upload-1/64", "user-1", false},
		{"too few segments", "avatars/user-1", "user-1", false},
		{"wrong root prefix", "notavatars/user-1/upload-1", "user-1", false},
		{"empty upload id segment", "avatars/user-1/", "user-1", false},
		{"empty key", "", "user-1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := avatarKeyOwnedBy(tt.key, tt.userID); got != tt.want {
				t.Errorf("avatarKeyOwnedBy(%q, %q) = %v, want %v", tt.key, tt.userID, got, tt.want)
			}
		})
	}
}

func TestAvatarThumbnailKey(t *testing.T) {
	got := avatarThumbnailKey("avatars/user-1/upload-1", 64)
	want := "avatars/user-1/upload-1/64"
	if got != want {
		t.Errorf("avatarThumbnailKey = %q, want %q", got, want)
	}
}

// TestCropAndResize_ProducesExactSquareDimensions confirms both a
// wide and a tall source image get center-cropped to a square and scaled
// to exactly the requested size — the DoD's "available in two sizes"
// claim, at the pixel-dimension level.
func TestCropAndResize_ProducesExactSquareDimensions(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
	}{
		{"wide", 400, 200},
		{"tall", 200, 400},
		{"already square", 300, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := image.NewRGBA(image.Rect(0, 0, tt.width, tt.height))
			for _, size := range []int{avatarThumbnailSize, avatarStandardSize} {
				out := cropAndResize(src, size)
				if out.Bounds().Dx() != size || out.Bounds().Dy() != size {
					t.Errorf("cropAndResize(%dx%d, %d) bounds = %v, want %dx%d", tt.width, tt.height, size, out.Bounds(), size, size)
				}
			}
		})
	}
}

// TestEncodeJPEG_RoundTrips confirms the encoded bytes are themselves a
// valid, decodable JPEG of the expected size — not just that encoding
// didn't return an error.
func TestEncodeJPEG_RoundTrips(t *testing.T) {
	src := cropAndResize(image.NewRGBA(image.Rect(0, 0, 500, 300)), avatarStandardSize)
	data, err := encodeJPEG(src, avatarJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG: unexpected error: %v", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode encoded jpeg: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want %q", format, "jpeg")
	}
	if cfg.Width != avatarStandardSize || cfg.Height != avatarStandardSize {
		t.Errorf("dimensions = %dx%d, want %dx%d", cfg.Width, cfg.Height, avatarStandardSize, avatarStandardSize)
	}
}
