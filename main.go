package main

import (
	"flag"
	"fmt"
	"regexp"

	"log"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
)

// Commit The Git SHA
var Commit = func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}()

func main() {
	src := flag.String("src", "", "source queue")
	dest := flag.String("dest", "", "destination queue")
	clients := flag.Int("clients", 1, "number of clients")
	limit := flag.Int("limit", -1, "limit number of messages moved")
	include := flag.String("include", "", "don't exclude message that match the specified pattern")
	version := flag.Bool("version", false, "display the version")
	flag.Parse()

	if *version {
		fmt.Printf("sqsmv/%s\n", Commit)
		os.Exit(0)
	}

	includeRegex, err := regexp.Compile(*include)

	if *src == "" || *dest == "" || *clients < 1 || err != nil {
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("source queue : %v", *src)
	log.Printf("destination queue : %v", *dest)
	log.Printf("number of clients : %v", *clients)
	log.Printf("limit : %v", *limit)
	log.Printf("include : %v", *include)

	// enable automatic use of AWS_PROFILE like awscli and other tools do.
	opts := session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}

	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		panic(err)
	}

	maxMessages := int64(10)
	waitTime := int64(0)
	messageAttributeNames := aws.StringSlice([]string{"All"})

	rmin := &sqs.ReceiveMessageInput{
		QueueUrl:              src,
		MaxNumberOfMessages:   &maxMessages,
		WaitTimeSeconds:       &waitTime,
		MessageAttributeNames: messageAttributeNames,
	}

	var messagesMoved atomic.Int64

	if *include != "" {
		visibilityTimeout := int64(120)
		rmin.VisibilityTimeout = &visibilityTimeout
	}

	var wg sync.WaitGroup

	for i := 1; i <= *clients; i++ {
		wg.Add(i)
		go transferMessages(sess, rmin, dest, &wg, *limit, &messagesMoved, includeRegex)
	}
	wg.Wait()
	log.Printf("all done, moved %v messages", messagesMoved.Load())
}

// transferMessages loops, transferring a number of messages from the src to the dest at an interval.
func transferMessages(theSession *session.Session, rmin *sqs.ReceiveMessageInput, dest *string, wgOuter *sync.WaitGroup, limit int, messagesMoved *atomic.Int64, includeRegex *regexp.Regexp) {
	client := sqs.New(theSession)

	lastMessageCount := int(1)

	defer wgOuter.Done()

	// loop as long as there are messages on the queue
	for {
		resp, err := client.ReceiveMessage(rmin)

		if err != nil {
			panic(err)
		}

		if limit != -1 && int(messagesMoved.Load()) > limit-1 {
			log.Printf("done")
			return
		}

		if lastMessageCount == 0 && len(resp.Messages) == 0 {
			// no messages returned twice now, the queue is probably empty
			log.Printf("done")
			return
		}

		lastMessageCount = len(resp.Messages)
		log.Printf("received %v messages...", len(resp.Messages))

		var wg sync.WaitGroup
		wg.Add(len(resp.Messages))

		for _, m := range resp.Messages {
			go func(m *sqs.Message) {
				defer wg.Done()

				if !includeRegex.MatchString(*m.Body) {
					return
				}

				// write the message to the destination queue
				smi := sqs.SendMessageInput{
					MessageAttributes: m.MessageAttributes,
					MessageBody:       m.Body,
					QueueUrl:          dest,
				}

				if limit != -1 && int(messagesMoved.Load()) > limit-1 {
					return
				}

				_, err := client.SendMessage(&smi)

				if err != nil {
					log.Printf("ERROR sending message to destination %v", err)
					return
				}

				messagesMoved.Add(1)

				// message was sent, dequeue from source queue
				dmi := &sqs.DeleteMessageInput{
					QueueUrl:      rmin.QueueUrl,
					ReceiptHandle: m.ReceiptHandle,
				}

				if _, err := client.DeleteMessage(dmi); err != nil {
					log.Printf("ERROR dequeueing message ID %v : %v",
						*m.ReceiptHandle,
						err)
				}
			}(m)
		}

		// wait for all jobs from this batch...
		wg.Wait()
	}
}
