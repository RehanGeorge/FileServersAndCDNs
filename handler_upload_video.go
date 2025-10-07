package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Upload limit
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30) // 1GB

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	// Implement upload logic
	videoFile, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get file from form", err)
		return
	}

	defer videoFile.Close()

	// Validate the video
	mediaType := fileHeader.Header.Get("Content-Type")
	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}

	switch mediaType {
	case "video/mp4":
		// Save uploaded file to temporary file on disk
		tempFile, err := os.CreateTemp(cfg.assetsRoot, "tubely-upload.mp4")
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
			return
		}
		defer os.Remove(tempFile.Name())
		defer tempFile.Close()

		io.Copy(tempFile, videoFile)

		// Reset the file pointer to the beginning of the file
		tempFile.Seek(0, io.SeekStart)

		// Upload to S3
		s3Key := make([]byte, 32)
		_, err = rand.Read(s3Key)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, "Couldn't generate random S3 key", err)
			return
		}

		s3KeyString := base64.RawURLEncoding.EncodeToString(s3Key) + ".mp4"
		_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
			Bucket:      &cfg.s3Bucket,
			Key:         &s3KeyString,
			Body:        tempFile,
			ContentType: &mediaType,
		})
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
			return
		}

		// Update the video URL in the database
		videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3KeyString)

		fmt.Printf("Uploaded video to S3 at location %s\n", videoURL)

		video.VideoURL = &videoURL
		err = cfg.db.UpdateVideo(video)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, "Couldn't update video metadata", err)
			return
		}

		respondWithJSON(w, http.StatusOK, map[string]string{"videoURL": videoURL})
		return
	default:
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported video format", err)
		return
	}
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Prepare the ffprobe command
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	// Set the Stdout field to a pointer
	var out bytes.Buffer
	cmd.Stdout = &out

	// Run the command
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	// Unmarshal the stdout of the command into a JSON struct
	type FFProbeStream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type FFProbeResult struct {
		Streams []FFProbeStream `json:"streams"`
	}

	var result FFProbeResult
	err = json.Unmarshal(out.Bytes(), &result)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal ffprobe output: %w", err)
	}

	if len(result.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	width := result.Streams[0].Width
	height := result.Streams[0].Height
	if width == 0 || height == 0 {
		return "", fmt.Errorf("invalid video dimensions")
	}

	// Calculate the aspect ratio

	// Use big.Rat for accurate ratio calculation to avoid floating point errors
	ratio := big.NewRat(int64(width), int64(height))

	// Define target ratios
	sixteenNine := big.NewRat(16, 9)
	nineSixteen := big.NewRat(9, 16)

	// Compare the calculated ratio to the targets
	if ratio.Cmp(sixteenNine) == 0 {
		return "16:9", nil
	} else if ratio.Cmp(nineSixteen) == 0 {
		return "9:16", nil
	} else {
		// If neither standard ratio matches, check for near-ratios common in video (e.g., 1920x1080)
		// We'll calculate the difference to see if it's "close enough" (for common dimensions like 1920x1080 vs 1.7777)
		// 16/9 is ~1.777
		if float64(width)/float64(height) > 1.7 && float64(width)/float64(height) < 1.78 {
			return "16:9", nil
		}

		// 9/16 is ~0.5625
		if float64(width)/float64(height) < 0.6 && float64(width)/float64(height) > 0.5 {
			return "9:16", nil
		}

		// If neither standard ratio is matched closely, return "other"
		return "other", nil
	}
}
