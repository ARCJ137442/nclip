package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

type S3Store struct {
	bucket     string
	prefix     string
	tempDir    string
	client     *s3.Client
	writerMu   sync.Mutex
	writerHeld bool
	writerETag *string
}

// NewS3Store creates a new S3Store instance
func NewS3Store(bucket, prefix string, tempDir ...string) (*S3Store, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3 bucket name must not be empty")
	}
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	directory := os.TempDir()
	if len(tempDir) > 0 && tempDir[0] != "" {
		directory = tempDir[0]
	}
	return &S3Store{bucket: bucket, prefix: prefix, tempDir: directory, client: client}, nil
}

func (s *S3Store) Store(paste *models.Paste) error {
	return s.storePasteMetadata(paste, "")
}

func (s *S3Store) StorePasteMetadata(paste *models.Paste, token string) error {
	return s.storePasteMetadata(paste, token)
}

func (s *S3Store) storePasteMetadata(paste *models.Paste, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.checkPasteSlugReservation(ctx, paste.ID, token); err != nil {
		return err
	}
	// Store metadata
	metaKey := applyS3Prefix(s.prefix, paste.ID+".json")
	metaData, err := json.MarshalIndent(paste, "", "  ")
	if err != nil {
		log.Printf("[ERROR] S3 Store: failed to marshal metadata for %s: %v", paste.ID, err)
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(metaKey),
		Body:   bytes.NewReader(metaData),
	})
	if err != nil {
		log.Printf("[ERROR] S3 Store: failed to put metadata for %s: %v", paste.ID, err)
		if utils.IsDebugEnabled() {
			log.Printf("[DEBUG] S3 bucket: %s, prefix: %s, metaKey: %s", s.bucket, s.prefix, metaKey)
			for _, e := range os.Environ() {
				log.Printf("[ENV] %s", e)
			}
		}
		if awsErr, ok := err.(interface {
			ErrorCode() string
			ErrorMessage() string
		}); ok {
			log.Printf("[AWS ERROR] Code: %s, Message: %s", awsErr.ErrorCode(), awsErr.ErrorMessage())
		}
	}
	return err
}

func (s *S3Store) Get(id string) (*models.Paste, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	metaKey := applyS3Prefix(s.prefix, id+".json")
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(metaKey),
	})
	if err != nil {
		// Map AWS not-found errors to storage.ErrNotFound
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			code := apiErr.ErrorCode()
			if code == "NoSuchKey" || code == "NotFound" || code == "404" {
				return nil, ErrNotFound
			}
		}
		if strings.Contains(err.Error(), "StatusCode: 404") || strings.Contains(err.Error(), "NotFound") {
			return nil, ErrNotFound
		}
		log.Printf("[ERROR] S3 Get: failed to get metadata for %s: %v", id, err)
		return nil, err
	}
	defer func() {
		if cerr := obj.Body.Close(); cerr != nil {
			log.Printf("[WARN] S3 Get: failed to close response body for %s: %v", id, cerr)
		}
	}()
	metaData, err := io.ReadAll(obj.Body)
	if err != nil {
		log.Printf("[ERROR] S3 Get: failed to read metadata body for %s: %v", id, err)
		return nil, err
	}
	var paste models.Paste
	if err := json.Unmarshal(metaData, &paste); err != nil {
		log.Printf("[ERROR] S3 Get: failed to unmarshal metadata for %s: %v", id, err)
		return nil, err
	}
	if paste.IsExpired() && !paste.IsEphemeral() {
		log.Printf("[INFO] S3 Get: paste %s is expired", id)
		// Attempt to delete expired objects (best-effort)
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(applyS3Prefix(s.prefix, id)),
		}); err != nil {
			log.Printf("[WARN] S3 Get: failed to delete expired content for %s: %v", id, err)
		}
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(applyS3Prefix(s.prefix, id+".json")),
		}); err != nil {
			log.Printf("[WARN] S3 Get: failed to delete expired metadata for %s: %v", id, err)
		}
		return nil, ErrNotFound
	}
	return &paste, nil
}

func (s *S3Store) readMetadata(id string) (*models.Paste, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, id+".json")),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			code := apiErr.ErrorCode()
			if code == "NoSuchKey" || code == "NotFound" || code == "404" {
				return nil, ErrNotFound
			}
		}
		if strings.Contains(err.Error(), "StatusCode: 404") || strings.Contains(err.Error(), "NotFound") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, err
	}
	var paste models.Paste
	if err := json.Unmarshal(data, &paste); err != nil {
		return nil, err
	}
	return &paste, nil
}

// ListEphemeralPastes is an internal cleanup scan. Ordinary listings must not
// expose channel content.
func (s *S3Store) ListEphemeralPastes() ([]models.Paste, error) {
	prefix := s.prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var token *string
	var pastes []models.Paste
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("list ephemeral paste metadata: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			key := strings.TrimPrefix(*object.Key, prefix)
			if strings.Contains(key, "/") || !strings.HasSuffix(key, ".json") {
				continue
			}
			id := strings.TrimSuffix(key, ".json")
			if !utils.IsValidSlug(id) {
				continue
			}
			paste, err := s.readMetadata(id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if paste != nil && paste.IsEphemeral() {
				pastes = append(pastes, *paste)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated || page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}
	return pastes, nil
}

func (s *S3Store) Exists(id string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, key := range []string{id, id + ".json", ChannelContentTempPrefix + id + ".pending", ChannelContentTempPrefix + id + ChannelContentReservationSuffix, PasteSlugReservationPrefix + id + ".lock"} {
		exists, err := s.objectExists(ctx, applyS3Prefix(s.prefix, key))
		if err != nil {
			log.Printf("[WARN] S3 Exists: failed to check %s: %v", key, err)
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (s *S3Store) ReservePasteSlug(id string) (string, func() error, error) {
	if !utils.IsValidSlug(id) {
		return "", nil, fmt.Errorf("invalid paste slug")
	}
	exists, err := s.Exists(id)
	if err != nil {
		return "", nil, err
	}
	if exists {
		return "", nil, ErrSlugReserved
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", nil, fmt.Errorf("create S3 paste slug reservation token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	key := applyS3Prefix(s.prefix, PasteSlugReservationPrefix+id+".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          strings.NewReader(token),
		ContentLength: aws.Int64(int64(len(token))),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "PreconditionFailed" || apiErr.ErrorCode() == "ConditionalRequestConflict") {
			return "", nil, ErrSlugReserved
		}
		return "", nil, fmt.Errorf("reserve S3 paste slug: %w", err)
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer releaseCancel()
			_, releaseErr = s.client.DeleteObject(releaseCtx, &s3.DeleteObjectInput{
				Bucket:  aws.String(s.bucket),
				Key:     aws.String(key),
				IfMatch: result.ETag,
			})
			if releaseErr != nil {
				releaseErr = fmt.Errorf("release S3 paste slug reservation: %w", releaseErr)
			}
		})
		return releaseErr
	}
	return token, release, nil
}

func (s *S3Store) objectExists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *S3Store) checkPasteSlugReservation(ctx context.Context, id, token string) error {
	key := applyS3Prefix(s.prefix, PasteSlugReservationPrefix+id+".lock")
	if token == "" {
		exists, err := s.objectExists(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			return ErrSlugReserved
		}
		return nil
	}
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isS3NotFound(err) {
			return ErrSlugReservationLost
		}
		return err
	}
	defer object.Body.Close()
	content, err := io.ReadAll(io.LimitReader(object.Body, int64(len(token))+1))
	if err != nil {
		return err
	}
	if string(content) != token {
		return ErrSlugReservationLost
	}
	return nil
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NoSuchKey" || code == "NotFound" || code == "404" {
			return true
		}
	}
	return strings.Contains(err.Error(), "StatusCode: 404") || strings.Contains(err.Error(), "NotFound")
}

func (s *S3Store) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, id)),
	}); err != nil {
		log.Printf("[ERROR] S3 Delete: failed to delete content for %s: %v", id, err)
		return fmt.Errorf("failed to delete content for %s: %w", id, err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, id+".json")),
	}); err != nil {
		log.Printf("[ERROR] S3 Delete: failed to delete metadata for %s: %v", id, err)
		return fmt.Errorf("failed to delete metadata for %s: %w", id, err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, ChannelContentTempPrefix+id+".pending")),
	}); err != nil {
		return fmt.Errorf("failed to delete pending channel content for %s: %w", id, err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, ChannelContentTempPrefix+id+ChannelContentReservationSuffix)),
	}); err != nil {
		return fmt.Errorf("failed to delete channel slug reservation for %s: %w", id, err)
	}
	return nil
}

func (s *S3Store) IncrementReadCount(id string) error {
	paste, err := s.Get(id)
	if err != nil {
		return err
	}
	paste.ReadCount++
	return s.Store(paste)
}

func (s *S3Store) StoreContent(id string, content []byte) error {
	return s.StoreContentStream(id, bytes.NewReader(content), int64(len(content)))
}

// StoreContentStream sends seekable streams directly to S3 and stages other
// readers in a bounded temporary file so the SDK can safely replay them.
func (s *S3Store) StoreContentStream(id string, content io.Reader, size int64) error {
	if !utils.IsValidSlug(id) {
		return s.storeContentStreamWithSlugReservation(id, "", content, size)
	}
	token, release, err := s.ReservePasteSlug(id)
	if err != nil {
		return err
	}
	storeErr := s.StorePasteContentStream(id, token, content, size)
	releaseErr := release()
	if storeErr != nil {
		return storeErr
	}
	return releaseErr
}

func (s *S3Store) StorePasteContentStream(id, token string, content io.Reader, size int64) error {
	return s.storeContentStreamWithSlugReservation(id, token, content, size)
}

func (s *S3Store) storeContentStreamWithSlugReservation(id, token string, content io.Reader, size int64) error {
	reservationCtx, reservationCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := s.checkPasteSlugReservation(reservationCtx, id, token); err != nil {
		reservationCancel()
		return err
	}
	reservationCancel()
	reserved, err := s.channelSlugReserved(id)
	if err != nil {
		return err
	}
	if reserved {
		return ErrSlugReserved
	}
	body, cleanup, err := s.channelUploadBody(content, size)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.checkPasteSlugReservation(ctx, id, token); err != nil {
		return err
	}
	reserved, err = s.channelSlugReservedWithContext(ctx, id)
	if err != nil {
		return err
	}
	if reserved {
		return ErrSlugReserved
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(applyS3Prefix(s.prefix, id)),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		log.Printf("[ERROR] S3 StoreContentStream: failed to put content for %s: %v", id, err)
		if utils.IsDebugEnabled() {
			log.Printf("[DEBUG] S3 bucket: %s, prefix: %s, key: %s", s.bucket, s.prefix, applyS3Prefix(s.prefix, id))
			for _, e := range os.Environ() {
				log.Printf("[ENV] %s", e)
			}
		}
		if awsErr, ok := err.(interface {
			ErrorCode() string
			ErrorMessage() string
		}); ok {
			log.Printf("[AWS ERROR] Code: %s, Message: %s", awsErr.ErrorCode(), awsErr.ErrorMessage())
		}
	}
	return err
}

func (s *S3Store) channelSlugReserved(id string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.channelSlugReservedWithContext(ctx, id)
}

func (s *S3Store) channelSlugReservedWithContext(ctx context.Context, id string) (bool, error) {
	for _, key := range []string{
		ChannelContentTempPrefix + id + ".pending",
		ChannelContentTempPrefix + id + ChannelContentReservationSuffix,
	} {
		exists, err := s.objectExists(ctx, applyS3Prefix(s.prefix, key))
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (s *S3Store) StoreChannelContentStream(id string, content io.Reader, size int64) error {
	token, release, err := s.ReservePasteSlug(id)
	if err != nil {
		return err
	}
	storeErr := s.StoreChannelContentStreamReserved(id, token, content, size)
	releaseErr := release()
	if storeErr != nil {
		return storeErr
	}
	return releaseErr
}

func (s *S3Store) StoreChannelContentStreamReserved(id, token string, content io.Reader, size int64) error {
	if !utils.IsValidSlug(id) {
		return fmt.Errorf("invalid channel content id")
	}
	// PasteService owns the per-slug reservation for the full create transaction;
	// check only finalization state here so the owning reservation is not rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.checkPasteSlugReservation(ctx, id, token); err != nil {
		return err
	}
	for _, key := range []string{id + ".json", ChannelContentTempPrefix + id + ".pending", ChannelContentTempPrefix + id + ChannelContentReservationSuffix} {
		exists, err := s.objectExists(ctx, applyS3Prefix(s.prefix, key))
		if err != nil {
			return err
		}
		if exists {
			return ErrSlugReserved
		}
	}
	contentExists, err := s.objectExistsWithTimeout(applyS3Prefix(s.prefix, id))
	if err != nil {
		return err
	}
	if contentExists {
		return ErrSlugReserved
	}
	reservationKey := applyS3Prefix(s.prefix, ChannelContentTempPrefix+id+ChannelContentReservationSuffix)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(reservationKey),
		Body:          strings.NewReader(id),
		ContentLength: aws.Int64(int64(len(id))),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		return fmt.Errorf("reserve S3 channel slug: %w", err)
	}
	reservationPending := true
	defer func() {
		if reservationPending {
			_ = s.deleteObject(reservationKey)
		}
	}()
	body, cleanup, err := s.channelUploadBody(content, size)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(applyS3Prefix(s.prefix, ChannelContentTempPrefix+id+".pending")),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return err
	}
	if err := s.deleteObject(reservationKey); err != nil {
		return fmt.Errorf("release S3 channel slug reservation: %w", err)
	}
	reservationPending = false
	return nil
}

func (s *S3Store) objectExistsWithTimeout(key string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.objectExists(ctx, key)
}

func (s *S3Store) channelUploadBody(content io.Reader, size int64) (io.Reader, func(), error) {
	if size < 0 || size == math.MaxInt64 {
		return nil, func() {}, fmt.Errorf("invalid channel content size")
	}
	if readerAt, ok := content.(io.ReaderAt); ok {
		return io.NewSectionReader(readerAt, 0, size), func() {}, nil
	}
	tmp, err := os.CreateTemp(s.tempDir, ChannelContentTempPrefix+"s3-upload-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create bounded S3 channel staging file: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	written, err := io.Copy(tmp, io.LimitReader(content, size+1))
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if written != size {
		cleanup()
		return nil, func() {}, fmt.Errorf("channel content size mismatch: expected %d bytes, got %d", size, written)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return tmp, cleanup, nil
}

func (s *S3Store) CommitChannelContent(id string) error {
	return s.commitChannelContent(id, "")
}

func (s *S3Store) CommitChannelContentReserved(id, token string) error {
	return s.commitChannelContent(id, token)
}

func (s *S3Store) commitChannelContent(id, token string) error {
	if !utils.IsValidSlug(id) {
		return fmt.Errorf("invalid channel content id")
	}
	stageKey := applyS3Prefix(s.prefix, ChannelContentTempPrefix+id+".pending")
	contentKey := applyS3Prefix(s.prefix, id)
	copySource := url.PathEscape(s.bucket) + "/" + strings.ReplaceAll(url.PathEscape(stageKey), "%2F", "/")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if token != "" {
		if err := s.checkPasteSlugReservation(ctx, id, token); err != nil {
			return err
		}
	}
	if _, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(contentKey),
		CopySource: aws.String(copySource),
	}); err != nil {
		return fmt.Errorf("publish channel content in S3: %w", err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(stageKey)}); err != nil {
		return fmt.Errorf("remove published channel staging object: %w", err)
	}
	return nil
}

func (s *S3Store) SweepPendingChannelContent() error {
	prefix := applyS3Prefix(s.prefix, ChannelContentTempPrefix)
	var token *string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		cancel()
		if err != nil {
			return fmt.Errorf("list pending channel content in S3: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			key := strings.TrimPrefix(*object.Key, applyS3Prefix(s.prefix, ""))
			if !strings.HasPrefix(key, ChannelContentTempPrefix) {
				continue
			}
			name := strings.TrimPrefix(key, ChannelContentTempPrefix)
			if strings.HasSuffix(name, ChannelContentReservationSuffix) {
				if err := s.deleteObject(*object.Key); err != nil {
					return fmt.Errorf("remove abandoned channel slug reservation from S3: %w", err)
				}
				continue
			}
			if !strings.HasSuffix(name, ".pending") {
				if err := s.deleteObject(*object.Key); err != nil {
					return fmt.Errorf("remove orphan channel staging object from S3: %w", err)
				}
				continue
			}
			id := strings.TrimSuffix(name, ".pending")
			if !utils.IsValidSlug(id) {
				if err := s.deleteObject(*object.Key); err != nil {
					return err
				}
				continue
			}
			paste, getErr := s.Get(id)
			switch {
			case getErr == nil && paste.IsEphemeral():
				if err := s.Delete(id); err != nil {
					return fmt.Errorf("remove orphan ephemeral channel content %s from S3: %w", id, err)
				}
			case getErr == nil:
				// A normal share owns this slug. Remove only the pending
				// channel object and preserve its final content and metadata.
			case errors.Is(getErr, ErrNotFound):
				// Without matching metadata the final slug may belong to an
				// ordinary upload between content and metadata writes.
			default:
				return fmt.Errorf("inspect pending channel content %s in S3: %w", id, getErr)
			}
			if err := s.deleteObject(*object.Key); err != nil {
				return fmt.Errorf("remove channel content marker from S3: %w", err)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated || page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}
	return nil
}

func (s *S3Store) deleteObject(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *S3Store) AcquireChannelWriter() error {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if s.writerHeld {
		return fmt.Errorf("channel writer lock already held by this store")
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("create channel writer token: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(applyS3Prefix(s.prefix, ChannelWriterLockKey)),
		Body:          bytes.NewReader(token),
		ContentLength: aws.Int64(int64(len(token))),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		return fmt.Errorf("acquire S3 channel writer lock: %w", err)
	}
	s.writerHeld = true
	s.writerETag = response.ETag
	return nil
}

func (s *S3Store) OwnsChannelWriter() bool {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if !s.writerHeld || s.writerETag == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, ChannelWriterLockKey)),
	})
	return err == nil && result.ETag != nil && *result.ETag == *s.writerETag
}

func (s *S3Store) WithChannelWriter(run func() error) error {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if !s.writerHeld || s.writerETag == nil {
		return ErrChannelWriterNotOwned
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, ChannelWriterLockKey)),
	})
	cancel()
	if err != nil || result.ETag == nil || *result.ETag != *s.writerETag {
		return ErrChannelWriterNotOwned
	}
	return run()
}

func (s *S3Store) GetContent(id string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, id)),
	})
	if err != nil {
		log.Printf("[ERROR] S3 GetContent: failed to get content for %s: %v", id, err)
		return nil, err
	}
	defer func() {
		if cerr := obj.Body.Close(); cerr != nil {
			log.Printf("[WARN] S3 GetContent: failed to close response body for %s: %v", id, cerr)
		}
	}()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		log.Printf("[ERROR] S3 GetContent: failed to read content body for %s: %v", id, err)
		return nil, err
	}
	return data, nil
}

// OpenContent keeps the S3 response body open so callers can stream it.
func (s *S3Store) OpenContent(id string) (io.ReadCloser, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(applyS3Prefix(s.prefix, id)),
	})
	if err != nil {
		cancel()
		log.Printf("[ERROR] S3 OpenContent: failed to get content for %s: %v", id, err)
		return nil, 0, err
	}
	size := int64(0)
	if obj.ContentLength != nil {
		size = *obj.ContentLength
	}
	return &cancelReadCloser{ReadCloser: obj.Body, cancel: cancel}, size, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// StatContent checks if the object exists in S3 and returns its size.
func (s *S3Store) StatContent(id string) (bool, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := applyS3Prefix(s.prefix, id)
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			code := apiErr.ErrorCode()
			if code == "NotFound" || code == "NoSuchKey" || code == "404" {
				return false, 0, nil
			}
		}
		// Also check textual message for 404
		if strings.Contains(err.Error(), "StatusCode: 404") || strings.Contains(err.Error(), "NotFound") {
			return false, 0, nil
		}
		log.Printf("[WARN] S3 StatContent: error checking %s: %v", id, err)
		return false, 0, err
	}
	if head.ContentLength == nil {
		return true, 0, nil
	}
	return true, *head.ContentLength, nil
}

// GetContentPrefix retrieves up to n bytes from an S3 object using Range header.
func (s *S3Store) GetContentPrefix(id string, n int64) ([]byte, error) {
	if n <= 0 {
		return []byte{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := applyS3Prefix(s.prefix, id)
	// Range header is inclusive: bytes=0-(n-1)
	rangeHeader := fmt.Sprintf("bytes=0-%d", n-1)
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		log.Printf("[ERROR] S3 GetContentPrefix: failed to get object %s: %v", id, err)
		return nil, err
	}
	defer func() {
		if cerr := obj.Body.Close(); cerr != nil {
			log.Printf("[WARN] S3 GetContentPrefix: failed to close response body for %s: %v", id, cerr)
		}
	}()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		log.Printf("[ERROR] S3 GetContentPrefix: failed to read body for %s: %v", id, err)
		return nil, err
	}
	return data, nil
}

func (s *S3Store) Close() error {
	return s.ReleaseChannelWriter()
}

func (s *S3Store) ReleaseChannelWriter() error {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if !s.writerHeld {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.bucket),
		Key:     aws.String(applyS3Prefix(s.prefix, ChannelWriterLockKey)),
		IfMatch: s.writerETag,
	})
	if err != nil {
		return fmt.Errorf("release S3 channel writer lock: %w", err)
	}
	s.writerHeld = false
	s.writerETag = nil
	return nil
}
