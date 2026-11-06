package backup

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"dbbak/internal/metadata"
)

// 对象存储（S3 兼容）后端：阿里云 OSS、MinIO、AWS S3 共用同一实现。
//
// 与 FTP 的语义差异，必须体现在代码里而不是文档里：
//  1. 没有目录：对象键是扁平字符串，"bucket/backups/mysql/2026/01/x.sql" 里的斜杠只是前缀分隔符。
//  2. 不能"回读整文件算 SHA-256"：FTP 双倍传输还可接受，对象存储按流量计费，大备份回读成本过高。
//     因此以「服务端返回的长度 + MD5（ETag，仅单次 PUT 可信）」作完整性校验，
//     本地 SHA-256 仍然计算并登记，供恢复前复核与清理前身份确认。
//  3. 写测试不需要先建目录：上传一个探测对象并确认其长度即可。

const (
	// ossPartSize 是多段上传的分片大小。S3 兼容服务的分片数上限为 10000，
	// 16 MiB 允许单对象约 156 GiB；超过该体积会在上传时显式失败，不会静默截断。
	ossPartSize = 16 << 20
	// ossSmallFileLimit 以内的文件走单次 PutObject：这样 ETag 就是 MD5，可用于校验。
	// 超过该体积走多段上传，ETag 变成「分片 MD5 串 + -N」，退化为仅校验长度。
	ossSmallFileLimit = 64 << 20
	// ossReadLimit 是清理前回读重算 SHA-256 的体积上限，避免清理任务拉取超大对象。
	ossReadLimit = 4 << 20
)

// ObjectKey 把「前缀 + 相对路径」拼成对象键。
// 与 FTP 不同，这里不做"必须是绝对路径"的限制：空前缀表示直接写在桶根。
func ObjectKey(prefix, relative string) (string, error) {
	cleanPrefix := strings.Trim(strings.ReplaceAll(strings.TrimSpace(prefix), "\\", "/"), "/")
	raw := strings.ReplaceAll(strings.TrimSpace(relative), "\\", "/")
	// path.Clean("/"+raw) 会把 "../x" 折叠成 "/x"，看似安全，实际是静默放行了越界输入。
	// 对象存储前缀之外可能存着别的数据，必须显式拒绝任何上级目录段。
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", fmt.Errorf("对象路径包含非法上级目录: %q", relative)
		}
	}
	cleanRelative := path.Clean("/" + raw)
	if cleanRelative == "/" || cleanRelative == "." {
		return "", fmt.Errorf("对象键不能为空")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(cleanRelative, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\x00\r\n") {
			return "", fmt.Errorf("对象路径包含非法目录段: %q", segment)
		}
	}
	relativeKey := strings.TrimPrefix(cleanRelative, "/")
	if cleanPrefix == "" {
		return relativeKey, nil
	}
	return cleanPrefix + "/" + relativeKey, nil
}

// normalizeOSSPrefix 校验并规范化用户填写的前缀，供保存时入库。
func normalizeOSSPrefix(raw string) string {
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"), "/")
}

// ossClient 按存储位置构造 S3 客户端。
// 使用静态凭据而不是默认凭据链：凭据来自加密的 secrets 表，不能被执行器所在机器的
// 环境变量或实例角色悄悄替换，否则备份会写到另一个账号的桶里。
func ossClient(target metadata.StorageTarget, secret string) (*s3.Client, error) {
	endpoint := strings.TrimSpace(target.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("对象存储端点未配置")
	}
	if !strings.Contains(endpoint, "://") {
		scheme := "https"
		if !target.UseSSL {
			scheme = "http"
		}
		endpoint = scheme + "://" + endpoint
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("对象存储端点不是合法 URL: %w", err)
	}
	if strings.TrimSpace(target.Bucket) == "" {
		return nil, fmt.Errorf("对象存储桶（Bucket）未配置")
	}
	if strings.TrimSpace(target.Username) == "" || secret == "" {
		return nil, fmt.Errorf("对象存储 AccessKey ID / Secret 未配置")
	}
	region := strings.TrimSpace(target.Region)
	if region == "" {
		region = "us-east-1" // 多数 S3 兼容服务不校验地域，取一个中性默认值。
	}
	client := s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(strings.TrimSpace(target.Username), secret, ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: target.PathStyle,
		// 新版 SDK 默认给 PutObject 附加 CRC32 校验头，部分 S3 兼容实现不认；
		// 这里只保留「需要时才计算」，由我们自己控制请求内容。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return client, nil
}

// errObjectNotFound 表示对象确实不存在，与"读取失败"必须区分：
// 前者可以登记为已删除，后者必须保持记录原状等待重试。
var errObjectNotFound = errors.New("对象不存在")

func isObjectNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

// parseETagMD5 仅在 ETag 是 32 位十六进制时视作整文件 MD5。
// 多段上传的 ETag 形如 "d41d8cd98f00b204e9800998ecf8427e-3"，那是分片摘要的组合，
// 与整文件 MD5 无关；部分服务对 SSE-KMS 对象返回的也不是 MD5，一律不做比较。
func parseETagMD5(etag string) ([]byte, bool) {
	trimmed := strings.Trim(strings.TrimSpace(etag), `"`)
	if len(trimmed) != 32 {
		return nil, false
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// putObjectSingle 走单次 PutObject，使 ETag 等价于 MD5，便于上传后比对。
func putObjectSingle(ctx context.Context, client *s3.Client, bucket, key, localPath string, size int64, digest []byte) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("读取本地备份文件: %w", err)
	}
	defer file.Close()
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          file,
		ContentLength: aws.Int64(size),
		ContentMD5:    aws.String(base64.StdEncoding.EncodeToString(digest)),
	})
	if err != nil {
		return fmt.Errorf("上传对象 %s 失败: %w", key, err)
	}
	return nil
}

// putObjectMultipart 按固定分片上传。分片失败会放弃本次上传，
// 由对账流程清理残留分片（对象存储不会自行删除未完成的多段上传）。
func putObjectMultipart(ctx context.Context, client *s3.Client, bucket, key, localPath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("读取本地备份文件: %w", err)
	}
	defer file.Close()
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("创建多段上传 %s 失败: %w", key, err)
	}
	uploadID := created.UploadId
	abort := func(cause error) error {
		_, abortErr := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: uploadID,
		})
		if abortErr != nil {
			return fmt.Errorf("%w（放弃多段上传失败，需对账清理残留分片）", cause)
		}
		return cause
	}
	buffer := make([]byte, ossPartSize)
	parts := make([]types.CompletedPart, 0, 16)
	for number := int32(1); number <= 10000; number++ {
		read, readErr := io.ReadFull(file, buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return abort(fmt.Errorf("读取第 %d 个分片失败: %w", number, readErr))
		}
		if read == 0 {
			break
		}
		output, partErr := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(key),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(number),
			Body:          bytes.NewReader(buffer[:read]),
			ContentLength: aws.Int64(int64(read)),
		})
		if partErr != nil {
			return abort(fmt.Errorf("上传第 %d 个分片失败: %w", number, partErr))
		}
		parts = append(parts, types.CompletedPart{ETag: output.ETag, PartNumber: aws.Int32(number)})
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if number == 10000 {
			// 超过 S3 分片数上限，必须显式失败：静默截断会产生"看起来成功"的残废备份。
			return abort(fmt.Errorf("文件超过 %d MiB 的多段上传上限，请拆分备份或改用其他存储", 10000*ossPartSize/(1<<20)))
		}
	}
	if len(parts) == 0 {
		return abort(fmt.Errorf("备份文件为空，拒绝上传空对象"))
	}
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(fmt.Errorf("完成多段上传 %s 失败: %w", key, err))
	}
	return nil
}

// ossUploadVerified 上传本地备份并在服务端复核长度与摘要。
// 返回的对象键与校验说明用于运行日志：校验等级必须如实记录，
// 「长度一致」与「内容一致」不是同一个结论。
func ossUploadVerified(ctx context.Context, target metadata.StorageTarget, secret, localPath, relative string) (string, string, error) {
	client, err := ossClient(target, secret)
	if err != nil {
		return "", "", err
	}
	key, err := ObjectKey(target.Root, relative)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", "", fmt.Errorf("读取本地备份文件属性: %w", err)
	}
	size := info.Size()
	file, err := os.Open(localPath)
	if err != nil {
		return "", "", fmt.Errorf("读取本地备份文件: %w", err)
	}
	hasher := md5.New()
	if _, err = io.Copy(hasher, file); err != nil {
		file.Close()
		return "", "", fmt.Errorf("计算本地摘要: %w", err)
	}
	file.Close()
	digest := hasher.Sum(nil)

	uploadErr := error(nil)
	if size <= ossSmallFileLimit {
		// 小文件走单次 PutObject：ETag 即 MD5，可做内容级校验。
		uploadErr = putObjectSingle(ctx, client, target.Bucket, key, localPath, size, digest)
	} else {
		// 大文件必须多段上传，否则单请求超时与内存占用都不可控。
		uploadErr = putObjectMultipart(ctx, client, target.Bucket, key, localPath)
	}
	if uploadErr != nil {
		return "", "", uploadErr
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
	if err != nil {
		// 上传报成功但读不到对象：不能登记为成功，删除半成品后失败返回。
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
		return "", "", fmt.Errorf("上传后无法确认对象 %s: %w", key, err)
	}
	remoteSize := aws.ToInt64(head.ContentLength)
	if remoteSize != size {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
		return "", "", fmt.Errorf("上传后长度不一致：本地 %d 字节，远端 %d 字节", size, remoteSize)
	}
	remoteETag := aws.ToString(head.ETag)
	if remoteDigest, ok := parseETagMD5(remoteETag); ok {
		if hex.EncodeToString(remoteDigest) != hex.EncodeToString(digest) {
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
			return "", "", fmt.Errorf("上传后 MD5 不一致：本地 %s，远端 %s", hex.EncodeToString(digest), hex.EncodeToString(remoteDigest))
		}
		return key, "长度与 MD5（ETag）一致", nil
	}
	// 多段上传或服务端加密对象：ETag 不是整文件摘要，只能确认长度。
	// 这不是失败，但必须让调用方知道校验等级低于单次 PUT。
	return key, "长度一致（多段上传 ETag 非整文件摘要，未比对内容哈希）", nil
}

// ossDelete 删除对象。S3 语义下删除不存在的键同样返回成功，
// 因此这里不把 NoSuchKey 当错误，调用方按"幂等删除"处理即可。
func ossDelete(ctx context.Context, target metadata.StorageTarget, secret, relative string) (string, error) {
	client, err := ossClient(target, secret)
	if err != nil {
		return "", err
	}
	key, err := ObjectKey(target.Root, relative)
	if err != nil {
		return "", err
	}
	if _, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)}); err != nil {
		if isObjectNotFound(err) {
			return key, errObjectNotFound
		}
		return key, fmt.Errorf("删除对象 %s 失败: %w", key, err)
	}
	return key, nil
}

// ossHead 读取对象元信息，用于清理前确认对象仍存在且长度一致。
func ossHead(ctx context.Context, target metadata.StorageTarget, secret, relative string) (int64, string, error) {
	client, err := ossClient(target, secret)
	if err != nil {
		return 0, "", err
	}
	key, keyErr := ObjectKey(target.Root, relative)
	if keyErr != nil {
		return 0, "", keyErr
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
	if err != nil {
		if isObjectNotFound(err) {
			return 0, key, errObjectNotFound
		}
		return 0, key, fmt.Errorf("读取对象 %s 元信息失败: %w", key, err)
	}
	return aws.ToInt64(head.ContentLength), aws.ToString(head.ETag), nil
}

// ossDownloadHash 回读对象头部若干字节重算 SHA-256 的"前缀哈希"，仅在对象较小时整读。
// 返回值 ok=false 表示对象超过回读上限，调用方应退化为长度校验而不是判定失败。
func ossDownloadHash(ctx context.Context, target metadata.StorageTarget, secret, relative string, expectSize int64) (string, bool, error) {
	if expectSize > ossReadLimit {
		return "", false, nil
	}
	client, err := ossClient(target, secret)
	if err != nil {
		return "", false, err
	}
	key, keyErr := ObjectKey(target.Root, relative)
	if keyErr != nil {
		return "", false, keyErr
	}
	output, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
	if err != nil {
		if isObjectNotFound(err) {
			return "", false, errObjectNotFound
		}
		return "", false, fmt.Errorf("回读对象 %s 失败: %w", key, err)
	}
	defer output.Body.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, output.Body); err != nil {
		return "", false, fmt.Errorf("回读对象 %s 传输中断: %w", key, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), true, nil
}

// OSSWriteTest 对对象存储做真实写检查：写探测对象 → 读回长度 → 删除。
// 三个动作分别验证 PutObject / HeadObject / DeleteObject 权限，
// 缺少任一权限都在保存时暴露，而不是等到备份跑完一半才失败。
func OSSWriteTest(target metadata.StorageTarget, secret string) error {
	client, err := ossClient(target, secret)
	if err != nil {
		return err
	}
	probe := make([]byte, 8)
	if _, err = rand.Read(probe); err != nil {
		return err
	}
	payload := "dbbak probe " + hex.EncodeToString(probe)
	key, err := ObjectKey(target.Root, "/dbbak-write-test-"+hex.EncodeToString(probe)+".tmp")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key),
		Body: strings.NewReader(payload), ContentLength: aws.Int64(int64(len(payload))),
	}); err != nil {
		return fmt.Errorf("写测试失败（桶 %s 不存在、AccessKey 无写入权限或被拒绝）: %w", target.Bucket, err)
	}
	head, headErr := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
	if headErr != nil {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
		return fmt.Errorf("写测试对象读不回来（可能需要 s3:GetObject 或 s3:ListBucket 权限）: %w", headErr)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(payload)) {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
		return fmt.Errorf("写测试对象长度不一致：写入 %d，读回 %d", len(payload), aws.ToInt64(head.ContentLength))
	}
	if _, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("写测试对象删除失败（需要 s3:DeleteObject 权限）: %w", err)
	}
	return nil
}
