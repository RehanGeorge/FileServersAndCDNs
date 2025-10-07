package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

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
