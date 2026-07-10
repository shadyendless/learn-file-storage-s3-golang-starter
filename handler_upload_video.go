package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoFile := http.MaxBytesReader(w, r.Body, 1<<30)
	if videoFile == nil {
		respondWithError(w, http.StatusBadRequest, "Videos must be less than 1GB", nil)
		return
	}
	defer videoFile.Close()

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
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not own this video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	fileType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	if fileType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Video must be mp4", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err = io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	fastFilepath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	fastFile, err := os.Open(fastFilepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	defer fastFile.Close()

	aspect_ratio, err := getVideoAspectRatio(fastFilepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	bucket_name := ""
	switch aspect_ratio {
	case "16:9":
		bucket_name = "landscape"
	case "9:16":
		bucket_name = "portrait"
	default:
		bucket_name = "other"
	}

	fastFile.Seek(0, io.SeekStart)
	fileExtension := strings.TrimPrefix(fileType, "video/")
	randomNameBytes := make([]byte, 32)
	rand.Read(randomNameBytes)
	randomName := base64.RawURLEncoding.EncodeToString(randomNameBytes)
	fileName := fmt.Sprintf("%s/%s.%s", bucket_name, randomName, fileExtension)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileName,
		Body:        fastFile,
		ContentType: &fileType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	videoUrl := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, fileName)
	video.VideoURL = &videoUrl

	if err = cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

type CmdResult struct {
	Streams []struct {
		Index              int    `json:"index"`
		Width              int    `json:"width"`
		Height             int    `json:"height"`
		DisplayAspectRatio string `json:"display_aspect_ratio"`
	} `json:"streams"`
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	cmdOutput := bytes.Buffer{}
	cmd.Stdout = &cmdOutput

	if err := cmd.Run(); err != nil {
		return "", err
	}

	result := CmdResult{}
	if err := json.Unmarshal(cmdOutput.Bytes(), &result); err != nil {
		return "", err
	}

	return result.Streams[0].DisplayAspectRatio, nil
}

func processVideoForFastStart(filepath string) (string, error) {
	processedFilename := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", processedFilename)
	cmdOutput := bytes.Buffer{}
	cmd.Stdout = &cmdOutput

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return processedFilename, nil
}
