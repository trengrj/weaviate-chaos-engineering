package main

import (
	"context"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/semi-technologies/weaviate-go-client/v2/weaviate"
	"github.com/semi-technologies/weaviate/entities/models"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	if err := do(context.Background()); err != nil {
		log.Fatal(err)
	}
}

type counter struct {
	sync.Mutex
	val int
}

func (c *counter) Inc(val int) {
	c.Lock()
	defer c.Unlock()

	c.val += val
}

func (c *counter) Count() int {
	c.Lock()
	defer c.Unlock()

	return c.val
}

func do(ctx context.Context) error {
	batchSize, err := getIntVar("BATCH_SIZE")
	if err != nil {
		return err
	}

	size, err := getIntVar("SIZE")
	if err != nil {
		return err
	}

	origin, err := getStringVar("ORIGIN")
	if err != nil {
		return err
	}

	shards, err := getIntVar("SHARDS")
	if err != nil {
		return err
	}

	client, err := newClient(origin)
	if err != nil {
		return err
	}

	if err := client.Schema().AllDeleter().Do(ctx); err != nil {
		return err
	}

	if err := client.Schema().ClassCreator().WithClass(getClass(shards)).Do(ctx); err != nil {
		return err
	}

	ids := make([]strfmt.UUID, size)
	for i := range ids {
		ids[i] = strfmt.UUID(uuid.NewString())
	}

	log.Printf("starting async query load")
	stop := make(chan bool)
	count := &counter{}
	go queryLoad(stop, client, ids, count)

	beforeAll := time.Now()
	batcher := client.Batch().ObjectsBatcher()

	for count.Count() < size {
		for i := 0; i < batchSize; i++ {
			batcher = batcher.WithObject(&models.Object{
				ID:    ids[count.Count()+i],
				Class: "InvertedIndexOnly",
				Properties: map[string]interface{}{
					// a lot of words will create large objects which in turn means we will need to
					// flush frequently which in turn should mean that we will have to compact a
					// lot, hopefully producing the SEGFAULT
					"text": GetWords(250, 250),
				},
			})
		}

		before := time.Now()
		if res, err := batcher.Do(ctx); err != nil {
			return err
		} else {
			for _, c := range res {
				if c.Result != nil {
					if c.Result.Errors != nil && c.Result.Errors.Error != nil {
						return errors.Errorf("failed to create obj: %+v, with status: %v",
							c.Result.Errors.Error[0], c.Result.Status)
					}
				}
			}
		}

		log.Printf("%f%% complete - last batch took %s - total %s\n",
			float32(count.Count())/float32(size)*100,
			time.Since(before), time.Since(beforeAll))
		count.Inc(batchSize)
	}

	stop <- true

	return nil
}

func queryLoad(stop chan bool, client *weaviate.Client, ids []strfmt.UUID,
	counter *counter) {
	t := time.Tick(2 * time.Millisecond)

	i := 0
	for {
		select {
		case <-stop:
			log.Printf("stopping async query load")
			return
		case <-t:
			i++

			if i == 10 {
				// on every tenth request include a GraphQL request
				i = 0
				sendFilteredReadQuery(client)
			}

			// on every request include a REST "by id" request
			sendByIDQuery(client, ids, counter)

		}
	}
}

func sendFilteredReadQuery(client *weaviate.Client) {
	res, err := client.GraphQL().Get().Objects().WithClassName("InvertedIndexOnly").
		WithLimit(1000).
		WithFields("_additional{ id }").
		Do(context.Background())
	if err != nil {
		log.Fatalf("read query failed (request level): %v", err)
	}

	if len(res.Errors) > 0 {
		log.Fatalf("read query failed (application level): %v", res.Errors[0].Message)
	}

	objs := res.Data["Get"].(map[string]interface{})["InvertedIndexOnly"].([]interface{})
	log.Printf("query query returned %d results", len(objs))
}

func sendByIDQuery(client *weaviate.Client, ids []strfmt.UUID, count *counter) {
	c := count.Count()

	if c == 0 {
		return
	}
	id := string(ids[rand.Intn(c)])
	_, err := client.Data().ObjectsGetter().WithID(id).
		Do(context.Background())
	if err != nil {
		log.Fatalf("read query failed (request level): %v", err)
	}
}

func getIntVar(envName string) (int, error) {
	v := os.Getenv(envName)
	if v == "" {
		return 0, errors.Errorf("missing required variable %s", envName)
	}

	asInt, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}

	return asInt, nil
}

func getStringVar(envName string) (string, error) {
	v := os.Getenv(envName)
	if v == "" {
		return v, errors.Errorf("missing required variable %s", envName)
	}

	return v, nil
}

func getClass(shards int) *models.Class {
	return &models.Class{
		Class:      "InvertedIndexOnly",
		Vectorizer: "none",
		VectorIndexConfig: map[string]interface{}{
			"skip": true,
		},
		ShardingConfig: map[string]interface{}{
			"desiredCount": shards,
		},
		Properties: []*models.Property{
			{
				Name:          "text",
				DataType:      []string{"text"},
				IndexInverted: ptFalse(),
			},
		},
	}
}

func ptFalse() *bool {
	v := true
	return &v
}

func newClient(origin string) (*weaviate.Client, error) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}

	cfg := weaviate.Config{
		Host:   parsed.Host,
		Scheme: parsed.Scheme,
	}
	return weaviate.New(cfg), nil
}
