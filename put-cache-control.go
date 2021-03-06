package main

import (
	"bufio"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"gopkg.in/urfave/cli.v1"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func assumeRoleCrossAccount(role string) (*aws.Config, error) {
	security := sts.New(session.New())
	input := &sts.AssumeRoleInput{
		DurationSeconds: aws.Int64(3600),
		ExternalId:      aws.String("123ABC"),
		RoleArn:         &role,
		RoleSessionName: aws.String("PutCacheControlImpersonification"),
	}
	impersonated, err := security.AssumeRole(input)
	if err != nil {
		return nil, fmt.Errorf("assume role '%s' for cross-account access failed: %v", role, err)
	}

	c := *impersonated.Credentials
	tmpCreds := credentials.NewStaticCredentials(*c.AccessKeyId, *c.SecretAccessKey, *c.SessionToken)
	return aws.NewConfig().WithCredentials(tmpCreds), nil
}

// Find out the number of objects in the bucket
// func getBucketSize(svc cloudwatch.CloudWatch) (int64, error) {
func getBucketSize(bucketName string, conf *aws.Config) (int64, error) {
	svcCrossAccount := cloudwatch.New(session.New(), conf)
	dims := []*cloudwatch.Dimension{
		&cloudwatch.Dimension{Name: aws.String("BucketName"), Value: aws.String(bucketName)},
		&cloudwatch.Dimension{Name: aws.String("StorageType"), Value: aws.String("AllStorageTypes")},
	}
	req := cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/S3"),
		StartTime:  aws.Time(time.Now().Add(-time.Hour * 24 * 3)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int64(3600), // TODO try out Period: 86400 (one day)
		Statistics: []*string{aws.String("Maximum")},
		MetricName: aws.String("NumberOfObjects"),
		Dimensions: dims,
	}
	resp, err := svcCrossAccount.GetMetricStatistics(&req)
	if err != nil {
		return 0, fmt.Errorf("Failed to detect bucket '%s' size: %v", bucketName, err)
	}

	if len(resp.Datapoints) == 0 {
		return 0, fmt.Errorf("No object counting for source bucket possible. Remember to give reading user `cloudwatch:GetMetricData` permission on source bucket. And provide --cross-account-cloudwatch-role")
	}
	max := 0.0
	for _, dp := range resp.Datapoints {
		if *dp.Maximum > max {
			max = *dp.Maximum
		}
	}
	return int64(max), nil
}

// Quickly find out the size of the bucket to copy for a nice progress indicator.
// Side effect: modifies the context
func getExpectedSize(context *CopyContext) {
	var err error
	context.expectedObjects, err = getBucketSize(context.from, &aws.Config{})
	if err != nil && context.cloudwatchRole != "" {
		// Retry getBucketSize using assume role (cross-account),
		// first acquire temporary cross-account credentials (AWS STS)
		confCrossAccount, errRole := assumeRoleCrossAccount(context.cloudwatchRole)
		if errRole != nil {
			os.Stderr.WriteString(fmt.Sprintf("Failed to detect 'from' bucket size: %v\n", errRole))
			context.expectedObjects = 0 // unknown
			return
		}
		context.expectedObjects, err = getBucketSize(context.from, confCrossAccount)
	}
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("Failed to detect 'from' bucket size: %v\n", err))
		context.expectedObjects = 0 // unknown
		return
	}
	fmt.Printf("Objects in the 'from' bucket: %d\n", context.expectedObjects)
}

// listObjectsFromStdin reads from stdin one object name per line.
// Also supports wildcard * at the end of the name.
func listObjectsFromStdin(names chan<- string, context *CopyContext) {
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		name := strings.TrimSpace(input.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue // skip empty and comment lines
		}
		if strings.HasSuffix(name, "*") {
			prefix := name[:len(name)-1]
			if strings.HasPrefix(name, "/") {
				prefix = prefix[1:] // remove leading slash if any
			}
			listObjectsToCopy(names, context.from, "", prefix, context)
		} else {
			names <- name
		}
	}
}

func listObjectsToCopy(names chan<- string, bucketname, continueFromKey, prefix string, context *CopyContext) {
	// fmt.Printf("Batch size for list: %d\n", context.options.batchsize)
	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucketname),
		MaxKeys: aws.Int64(context.options.batchsize),
	}
	if continueFromKey != "" {
		input.StartAfter = &continueFromKey
	}
	if prefix != "" {
		input.Prefix = &prefix
	}

	err := context.s3svc.ListObjectsV2Pages(input,
		func(page *s3.ListObjectsV2Output, lastPage bool) bool {
			// Could use following if cloudwatch based metrics are not available:
			// atomic.AddInt64(&context.expectedObjects, int64(len(page.Contents)))
			for _, item := range page.Contents {
				names <- *item.Key
			}
			// stop pumping names once we have copied enough
			return context.copiedObjects < context.maxObjectsToCopy
		})
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("%s", err))
	}
}

func main() {
	app := cli.NewApp()
	app.Usage = "Set Cache-Control header for all objects in a s3 bucket. Optionally copies objects from another bucket."
	app.Version = "0.1"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "target-bucket, t",
			Usage: "where changes will happen: objects added or meta-data changed",
		},
		cli.StringFlag{
			Name:  "from-bucket, f",
			Usage: "if omitted, use in-place-copy (target-bucket=from-bucket)",
		},
		cli.StringFlag{
			Name:  "cache-control, c",
			Value: "max-age=31536000,public",
			Usage: "by default cache for one year",
		},
		cli.IntFlag{
			Name:  "parallelity, p",
			Value: 200,
			Usage: "number of workers to use",
		},
		cli.BoolFlag{
			Name:  "noop",
			Usage: "make no changes, just gather statistics",
		},
		cli.StringFlag{
			Name:  "exclude-pictures, e",
			Usage: "do not process picture object which names match regex",
		},
		cli.IntFlag{
			Name:  "first-n, n",
			Value: math.MaxInt64,
			Usage: "stop copy/process roughly after first n entries; skipped\n\tand ignored do not count",
		},
		cli.StringFlag{
			Name:  "continue, u",
			Usage: "do not start over, continue from given key",
		},
		cli.BoolFlag{
			Name:  "stdin",
			Usage: "take file names to copy from stdin",
		},
		cli.StringFlag{
			Name: "cross-account-cloudwatch-role, r",
			Usage: `

	Sometimes you need to copy objects between buckets from different accounts
	(cross-account), e.g. prod- vs. nonprod- account. Obviously you need to give
	the account you currently use write permission to the target bucket and read
	permission to the 'from' bucket. But to also have a correct progress bar for
	the long running copy operation, you need to give your account the permission
	to access cloudwatch metrics for the 'from' bucket.

			`,
		},
	}
	app.Action = func(c *cli.Context) error {
		context, _ := prepareContextFromCli(c)

		// set well below the typical ulimit of 1024 - TODO add to docs
		// to avoid "socket: too many open files".
		// Also fits AWS API limits, avoid "503 SlowDown: Please reduce your request rate."
		parallelity := c.GlobalInt("parallelity")

		names := make(chan string, context.options.batchsize*3) // enable uninterrupted stream of files to copy
		events := make(chan CopyResult, 10000)
		getExpectedSize(&context)

		context.wg.Add(parallelity)
		for gr := 1; gr <= parallelity; gr++ {
			go cpworker(&context, names, events)
		}
		waitStats := sync.WaitGroup{}
		waitStats.Add(1)
		go processStats(context.expectedObjects, events, &waitStats)

		if c.GlobalBool("stdin") {
			listObjectsFromStdin(names, &context)
		} else {
			listObjectsToCopy(names, context.from, c.GlobalString("continue"), "", &context)
		}
		close(names)
		context.wg.Wait()
		close(events)
		waitStats.Wait()
		fmt.Printf("\nDone.\n")
		return nil
	}
	app.Run(os.Args)
}

func CheckPublicCommentTmp() {
}

/* CopyContext defines context for running concurrent copy operations and remembers the progress */
type CopyContext struct {
	s3svc *s3.S3

	options CopyOptions

	target         string
	from           string
	newvalue       string
	exclude        regexp.Regexp
	cloudwatchRole string
	noop           bool

	maxObjectsToCopy int64
	expectedObjects  int64
	copiedObjects    int64

	wg sync.WaitGroup
}

type CopyOptions struct {
	target         string
	from           string
	newvalue       string
	exclude        regexp.Regexp
	cloudwatchRole string
	noop           bool
	batchsize      int64
}

type CopyResult struct {
	status, key, contenttype string
	err                      error
}

func prepareContext() (CopyContext, error) {
	// Session with the new library
	sess, err := session.NewSession() /*&aws.Config{
		Region: aws.String("eu-central-1")},
	)*/
	if err != nil {
		panic(fmt.Sprintf("Can not create AWS SDK session %s", err))
	}

	if len(os.Args) != 3 {
		panic("Please provide bucket name and desired Cache-Control setting")
	}
	return CopyContext{
		s3svc:           s3.New(sess),
		target:          os.Args[1],
		expectedObjects: 3867874,
		newvalue:        os.Args[2],
	}, nil
}

func prepareContextFromCli(c *cli.Context) (CopyContext, error) {
	// Session with the new library
	sess, err := session.NewSession() /*&aws.Config{
		Region: aws.String("eu-central-1")},
	)*/
	if err != nil {
		panic(fmt.Sprintf("Can not create AWS SDK session %s", err))
	}

	target := c.GlobalString("target-bucket")
	if target == "" {
		cli.ShowAppHelp(c)
		return CopyContext{}, cli.NewExitError("\n\nError: --target-bucket is a required flag\n", 1)
	}

	from := c.GlobalString("from-bucket")
	if from == "" {
		from = target
	}

	fmt.Printf("Copying   to %s\nCopying from %s\n", target, from)

	exclude_pattern := c.GlobalString("exclude-pictures")
	if exclude_pattern == "" {
		exclude_pattern = "^some-pattern-which-would-never-match$"
	}

	o := CopyOptions{}
	o.batchsize = c.GlobalInt64("first-n") / 2
	if o.batchsize > 1000 {
		o.batchsize = 1000
	}
	if o.batchsize < 10 {
		o.batchsize = 10
	}

	return CopyContext{
		s3svc:            s3.New(sess),
		target:           target,
		from:             from,
		noop:             c.GlobalBool("noop"),
		expectedObjects:  0,
		maxObjectsToCopy: c.GlobalInt64("first-n"),
		newvalue:         c.GlobalString("cache-control"),
		exclude:          *regexp.MustCompile(exclude_pattern),
		cloudwatchRole:   c.GlobalString("cross-account-cloudwatch-role"),
		options:          o,
	}, nil
}

func cpworker(context *CopyContext, names <-chan string, events chan<- CopyResult) {
	for {
		name, more := <-names
		if more {
			// fmt.Printf("Starting copy %v\n", name)
			events <- cp(context, name)
		} else {
			// fmt.Printf("\nNo more data in names channel.\n")
			context.wg.Done()
			return
		}
	}
}

func IsPicture(meta *s3.HeadObjectOutput) bool {
	switch *meta.ContentType {
	case
		"image/jpeg",
		"image/png":
		return true
	default:
		return false
	}
}

func str(o *s3.HeadObjectOutput) string {
	if o == nil {
		return "NotFound"
	} else {
		cache, ctype := "nil", "nil"
		if o.CacheControl != nil {
			cache = *o.CacheControl
		}
		if o.ContentType != nil {
			ctype = *o.ContentType
		}
		return fmt.Sprintf("%s, %s", cache, ctype)
	}
}

func cp(context *CopyContext, name string) CopyResult {
	//fmt.Println(context.target)
	//fmt.Println(url.PathEscape(name))
	// key := aws.String(url.PathEscape(name)),
	key := name
	res := CopyResult{status: "X", key: key}
	from, fromErr := context.s3svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(context.from),
		Key:    aws.String(key),
	})
	if fromErr != nil {
		res.err = fmt.Errorf("\naws sdk Head for `%s` failed: \n%T\n%v\n", key, fromErr, fromErr)
		return res
	}

	contenttype := from.ContentType

	target, targetErr := context.s3svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(context.target),
		Key:    aws.String(key),
	})
	if targetErr != nil {
		if aerr, ok := targetErr.(awserr.Error); ok {
			switch aerr.Code() {
			case "NotFound":
				target = nil
			default:
				os.Stderr.WriteString(fmt.Sprintf("\n***Missing target Head for `%s` failed (code %s): \n%T\n%v\n",
					key, aerr.Code(), targetErr, targetErr))
			}
		} else {
			res.err = fmt.Errorf("\naws sdk Head for target `%s` failed, can not recognize the aws return code: \n%T\n%v\n",
				key, fromErr, fromErr)
			return res
		}
	}

	// E - excluded pattern
	// . - skip, Cache-Control and Content-Type already set
	// X - type was not set, set to image/png
	// j - was image/jpeg; adjusted CacheControl
	// g - was image/png; adjusted CacheControl
	// P - pdf file; adjusted CacheControl
	// Y - other file type; adjusted CacheControl
	if context.exclude.MatchString(name) && IsPicture(from) {
		res.status = "E"
	} else if target != nil && target.CacheControl != nil && *target.CacheControl == context.newvalue &&
		target.ContentType != nil {
		res.status = "."
	} else if context.copiedObjects > context.maxObjectsToCopy {
		res.status = ","
	} else {
		if contenttype == nil {
			res.status = "X"
			contenttype = aws.String("image/png")
		} else {
			// DEBUG: fmt.Printf("\nkey %s, from: %s target: %s\n", key, str(from), str(target))
			// contenttype = contenttypes[0] // theoretically, there can be multiple HTTP headers with the same key
			// but lets assume, there is at most one
			if *contenttype == "image/png" {
				res.status = "g"
			} else if *contenttype == "image/jpeg" {
				res.status = "j"
			} else if *contenttype == "application/pdf" {
				res.status = "P"
			} else {
				res.status = "Y"
			}
		}

		src := fmt.Sprintf("%s/%s", context.from, url.PathEscape(name))
		inp := s3.CopyObjectInput{
			Bucket:            aws.String(context.target),
			CopySource:        &src,
			Key:               &name,
			CacheControl:      &context.newvalue,
			ContentType:       contenttype,
			MetadataDirective: aws.String("REPLACE"),
		}
		if !context.noop {
			_, err := context.s3svc.CopyObject(&inp)
			if err != nil {
				res.err = fmt.Errorf("Failed changing (inplace-copying) object: %v", err)
				return res
			}
		}
		atomic.AddInt64(&context.copiedObjects, 1)
	}

	if contenttype == nil {
		res.contenttype = ""
	} else {
		res.contenttype = *contenttype
	}
	return res
}

func processStats(expected int64, events <-chan CopyResult, running *sync.WaitGroup) {
	var processedObjects int64 // including ignored and skipped
	start := time.Now()
	statusStats := make(map[string]int)
	typeStats := make(map[string]int)
	last := ""
	every := time.NewTicker(12 * time.Second)

	showStats := func() {
		sec := time.Since(start).Seconds()
		o_s := float64(processedObjects) / sec
		expectedDuration := time.Duration(int(float64(expected-processedObjects)/o_s)) * time.Second
		days := int(expectedDuration.Hours() / 24)
		andHours := expectedDuration.Hours() - float64(days)*24
		eta := fmt.Sprintf("%dd %.1fh", days, andHours)
		if days == 0 {
			hours := int(expectedDuration.Minutes() / 60)
			andMinutes := expectedDuration.Minutes() - float64(hours)*60
			eta = fmt.Sprintf("%dh %.1fm", hours, andMinutes)
		}
		if expected < processedObjects {
			eta = "-"
		}

		fmt.Printf("\n%-30s Totals: %d/%d objects. Avg: %.2f obj/s. ETA: %v    \n",
			last, processedObjects, expected, o_s, eta,
		)
		fmt.Printf("\nContent-Type stats:\n")
		for k, v := range typeStats {
			fmt.Printf("%s %d\n", k, v)
		}
		fmt.Printf("\nCopy status stats:\n")
		for k, v := range statusStats {
			fmt.Printf("%s %d\n", k, v)
		}
	}

	fileToWrite := func(name string) *os.File {
		f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			os.Stderr.WriteString(fmt.Sprintf("Could not create file '%s'. Error: %v\n", name, err))
			panic(err)
		}
		return f
	}

	run := time.Now().Format("2006-01-02-150405")
	fList := fileToWrite(run + "-objects.log")
	defer fList.Close()
	fErrors := fileToWrite(run + "-error-keys.log")
	defer fErrors.Close()

	for {
		select {
		case <-every.C:
			showStats()
			time.Sleep(4 * time.Second) // give the user opportunity to read
		case event, more := <-events:
			if more {
				fmt.Fprintf(fList, "%s\t%s\t%s\n", event.status, event.contenttype, event.key)
				if event.err != nil {
					os.Stderr.WriteString(fmt.Sprintf("==> Failed processing '%s': %v\n", event.key, event.err))
					fmt.Fprintln(fErrors, event.key)
				}

				statusStats[event.status] += 1
				// extract interesting part before semicolon, like "mulitpart/package"
				// from `multipart/package; boundary="_-------------1437962543790"`
				ctype := strings.Split(event.contenttype, ";")[0]
				typeStats[ctype] += 1
				processedObjects += 1
				last = event.key
				fmt.Print(event.status)
			} else {
				fmt.Printf("\n\n## Event channel closed. Final statistics:\n")
				showStats()
				running.Done()
				return
			}
		}
	}

}
