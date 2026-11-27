// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A request for "mp3" used to be answered with a WAV file: the engines render WAV or PCM, and nothing converted it. Callers took the bytes at their word, so
// voice-studio saved RIFF data as <id>.mp3 and served it as audio/mpeg. A browser sniffs the content and plays it; CoreAudio (QuickTime, Music, Safari's engine)
// does not, and a lossless file passed through a player's mp3 path is where edges get chopped. A request for mp3 now gets an mp3.

// mp3Bitrate is the constant bit rate of the mp3 forge-tts makes. 192 kbit/s keeps speech transparent; it is not a quality setting a caller can change.
const mp3Bitrate = "192k"

// encodeMP3 turns a WAV file into an MP3 with ffmpeg's libmp3lame. The output goes through a file, not a pipe: only a seekable output lets ffmpeg write the
// Xing/LAME header with the real length and the encoder delay, which is what lets a player trim the encoder's padding exactly instead of guessing.
// A missing ffmpeg is an error the caller sees: the answer is never WAV bytes under an mp3 label.
func encodeMP3(ctx context.Context, wav []byte) ([]byte, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("mp3 output needs ffmpeg on the speech host and none was found: %w", err)
	}
	out, err := os.CreateTemp("", "forge-tts-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("mp3 output: temporary file: %w", err)
	}
	path := out.Name()
	out.Close()
	defer os.Remove(path)

	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, ffmpeg, "-v", "error", "-y", "-f", "wav", "-i", "pipe:0", "-map_metadata", "-1",
		"-codec:a", "libmp3lame", "-b:a", mp3Bitrate, "-write_xing", "1", "-f", "mp3", path)
	cmd.Stdin = bytes.NewReader(wav)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mp3 encode failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mp3 output: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("mp3 encode produced no audio")
	}
	return data, nil
}
