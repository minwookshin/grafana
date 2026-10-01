package sql

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/grafana/grafana/pkg/apimachinery/utils"
	"github.com/grafana/grafana/pkg/storage/unified/resource"
	"github.com/grafana/grafana/pkg/storage/unified/resourcepb"
	"github.com/grafana/grafana/pkg/storage/unified/sql/db"
	"github.com/grafana/grafana/pkg/storage/unified/sql/dbutil"
	"github.com/grafana/grafana/pkg/storage/unified/sql/sqltemplate"
)

var (
	_ resource.BlobSupport = (*backend)(nil)
	_ resource.BlobSupport = (*blobStore)(nil)
)

type blobStore struct {
	db      db.DB
	dialect sqltemplate.Dialect
}

func NewBlobStore(db db.DB, dialect sqltemplate.Dialect) resource.BlobSupport {
	return &blobStore{db: db, dialect: dialect}
}

func NewBlobStoreFromProvider(ctx context.Context, provider db.DBProvider) (resource.BlobSupport, error) {
	dbConn, err := provider.Init(ctx)
	if err != nil {
		return nil, fmt.Errorf("error initializing DB: %w", err)
	}
	dialect := sqltemplate.DialectForDriver(dbConn.DriverName())
	if dialect == nil {
		return nil, fmt.Errorf("unsupported database driver: %s", dbConn.DriverName())
	}
	return NewBlobStore(dbConn, dialect), nil
}

func (b *backend) SupportsSignedURLs() bool {
	b.logCall("SupportsSignedURLs")
	return false
}

func (b *backend) PutResourceBlob(ctx context.Context, req *resourcepb.PutBlobRequest) (*resourcepb.PutBlobResponse, error) {
	b.logCall("PutResourceBlob")
	return (&blobStore{db: b.db, dialect: b.dialect}).PutResourceBlob(ctx, req)
}

func (b *backend) GetResourceBlob(ctx context.Context, key *resourcepb.ResourceKey, info *utils.BlobInfo, mustProxy bool) (*resourcepb.GetBlobResponse, error) {
	b.logCall("GetResourceBlob")
	return (&blobStore{db: b.db, dialect: b.dialect}).GetResourceBlob(ctx, key, info, mustProxy)
}

func (b *blobStore) SupportsSignedURLs() bool {
	return false
}

func (b *blobStore) PutResourceBlob(ctx context.Context, req *resourcepb.PutBlobRequest) (*resourcepb.PutBlobResponse, error) {
	ctx, span := tracer.Start(ctx, "sql.backend.PutResourceBlob")
	defer span.End()

	if req.Method == resourcepb.PutBlobRequest_HTTP {
		return &resourcepb.PutBlobResponse{
			Error: resource.NewBadRequestError("signed url upload not supported"),
		}, nil
	}

	hasher := md5.New() // same as s3
	_, err := hasher.Write(req.Value)
	if err != nil {
		return nil, err
	}

	info := &utils.BlobInfo{
		UID:  uuid.NewV4().String(),
		Size: int64(len(req.Value)),
		Hash: hex.EncodeToString(hasher.Sum(nil)),
	}
	info.SetContentType(req.ContentType)

	if info.Size < 1 {
		return &resourcepb.PutBlobResponse{
			Error: resource.NewBadRequestError("empty content"),
		}, nil
	}

	// Insert the value
	err = b.db.WithTx(ctx, ReadCommitted, func(ctx context.Context, tx db.Tx) error {
		_, err := dbutil.Exec(ctx, tx, sqlResourceBlobInsert, sqlResourceBlobInsertRequest{
			SQLTemplate: sqltemplate.New(b.dialect),
			Now:         time.Now(),
			Info:        info,
			Key:         req.Resource,
			ContentType: req.ContentType,
			Value:       req.Value,
		})
		return err
	})

	if err != nil {
		return &resourcepb.PutBlobResponse{
			Error: resource.AsErrorResult(err),
		}, nil
	}
	return &resourcepb.PutBlobResponse{
		Uid:      info.UID,
		Size:     info.Size,
		MimeType: info.MimeType,
		Charset:  info.Charset,
		Hash:     info.Hash,
	}, nil
}

func (b *blobStore) GetResourceBlob(ctx context.Context, key *resourcepb.ResourceKey, info *utils.BlobInfo, mustProxy bool) (*resourcepb.GetBlobResponse, error) {
	ctx, span := tracer.Start(ctx, "sql.backend.GetResourceBlob")
	defer span.End()

	if info == nil {
		return &resourcepb.GetBlobResponse{
			Error: resource.NewBadRequestError("missing blob info"),
		}, nil
	}

	rsp := &resourcepb.GetBlobResponse{}
	err := b.db.WithTx(ctx, ReadCommitted, func(ctx context.Context, tx db.Tx) error {
		rows, err := dbutil.QueryRows(ctx, tx, sqlResourceBlobQuery, sqlResourceBlobQueryRequest{
			SQLTemplate: sqltemplate.New(b.dialect),
			Key:         key,
			UID:         info.UID, // optional
		})
		if err != nil {
			return err
		}
		if rows.Next() {
			uid := ""
			err = rows.Scan(&uid, &rsp.Value, &rsp.ContentType)
			if info.UID != "" && info.UID != uid {
				return fmt.Errorf("unexpected uid in result")
			}
			return err
		}
		rsp.Error = &resourcepb.ErrorResult{
			Code: http.StatusNotFound,
		}
		return err
	})
	if err != nil {
		rsp.Error = resource.AsErrorResult(err)
	}
	return rsp, nil
}
