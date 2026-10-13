package s3api

import "encoding/xml"

const (
	xmlns      = "http://s3.amazonaws.com/doc/2006-03-01/"
	timeFormat = "2006-01-02T15:04:05.000Z"
	ownerID    = "openchuspa"
)

type owner struct {
	ID          string
	DisplayName string
}

type listBucketsResult struct {
	XMLName xml.Name      `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Owner   owner         `xml:"Owner"`
	Buckets []bucketEntry `xml:"Buckets>Bucket"`
}

type bucketEntry struct {
	Name         string
	CreationDate string
}

type locationConstraint struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Region  string   `xml:",chardata"`
}

type listObjectsV2Result struct {
	XMLName               xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string
	Prefix                string
	Delimiter             string `xml:",omitempty"`
	StartAfter            string `xml:",omitempty"`
	MaxKeys               int
	EncodingType          string `xml:",omitempty"`
	KeyCount              int
	IsTruncated           bool
	ContinuationToken     string         `xml:",omitempty"`
	NextContinuationToken string         `xml:",omitempty"`
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type commonPrefix struct {
	Prefix string
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
	Bucket   string
	Key      string
	UploadID string `xml:"UploadId"`
}

type completeMultipartUpload struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int
		ETag       string
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName           xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
	Location          string
	Bucket            string
	Key               string
	ETag              string
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
	ChecksumType      string `xml:",omitempty"`
}

type listPartsResult struct {
	XMLName              xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
	Bucket               string
	Key                  string
	UploadID             string `xml:"UploadId"`
	PartNumberMarker     int
	NextPartNumberMarker int
	MaxParts             int
	IsTruncated          bool
	Parts                []partEntry `xml:"Part"`
}

type partEntry struct {
	PartNumber   int
	LastModified string
	ETag         string
	Size         int64
}
