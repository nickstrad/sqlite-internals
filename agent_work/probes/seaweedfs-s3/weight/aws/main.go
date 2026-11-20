package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://127.0.0.1:18333"),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("course", "course-secret", ""),
	})
	out, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	fmt.Println(out != nil, err)
}
