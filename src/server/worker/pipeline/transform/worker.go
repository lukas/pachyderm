package transform

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/types"
	"golang.org/x/sync/errgroup"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/limit"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pkg/pbutil"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/obj"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/server/pkg/work"
	"github.com/pachyderm/pachyderm/src/server/worker/common"
	"github.com/pachyderm/pachyderm/src/server/worker/driver"
	"github.com/pachyderm/pachyderm/src/server/worker/logs"
	"github.com/pachyderm/pachyderm/src/server/worker/server"
)

var (
	errDatumRecovered = errors.New("the datum errored, and the error was handled successfully")
)

// TODO: would be nice to have these have a deterministic ID rather than based
// off the subtask ID so we can shortcut processing if we get interrupted and
// restarted
func jobRecoveredDatumsTag(jobID string, subtaskID string) string {
	return fmt.Sprintf("%s-recovered-%s", jobTagPrefix(jobID), subtaskID)
}

func jobChunkTag(jobID string, subtaskID string) string {
	return fmt.Sprintf("%s-chunk-%s", jobTagPrefix(jobID), subtaskID)
}

func plusDuration(x *types.Duration, y *types.Duration) (*types.Duration, error) {
	var xd time.Duration
	var yd time.Duration
	var err error
	if x != nil {
		xd, err = types.DurationFromProto(x)
		if err != nil {
			return nil, err
		}
	}
	if y != nil {
		yd, err = types.DurationFromProto(y)
		if err != nil {
			return nil, err
		}
	}
	return types.DurationProto(xd + yd), nil
}

// mergeStats merges y into x
func mergeStats(x, y *DatumStats) error {
	if yps := y.ProcessStats; yps != nil {
		var err error
		xps := x.ProcessStats
		if xps.DownloadTime, err = plusDuration(xps.DownloadTime, yps.DownloadTime); err != nil {
			return err
		}
		if xps.ProcessTime, err = plusDuration(xps.ProcessTime, yps.ProcessTime); err != nil {
			return err
		}
		if xps.UploadTime, err = plusDuration(xps.UploadTime, yps.UploadTime); err != nil {
			return err
		}
		xps.DownloadBytes += yps.DownloadBytes
		xps.UploadBytes += yps.UploadBytes
	}

	x.DatumsProcessed += y.DatumsProcessed
	x.DatumsSkipped += y.DatumsSkipped
	x.DatumsFailed += y.DatumsFailed
	x.DatumsRecovered += y.DatumsRecovered
	if x.FailedDatumID == "" {
		x.FailedDatumID = y.FailedDatumID
	}
	return nil
}

// Worker handles a transform pipeline work subtask, then returns.
func Worker(driver driver.Driver, logger logs.TaggedLogger, subtask *work.Task, status *Status) error {
	// Handle 'process datum' tasks
	datumData, err := deserializeDatumData(subtask.Data)
	if err == nil {
		return status.withJob(datumData.JobID, func() error {
			logger = logger.WithJob(datumData.JobID)
			if err := logger.LogStep("datum task", func() error {
				return handleDatumTask(driver, logger, datumData, subtask.ID, status)
			}); err != nil {
				return err
			}

			subtask.Data, err = serializeDatumData(datumData)
			return err
		})
	}

	// Handle 'merge hashtrees' tasks
	mergeData, err := deserializeMergeData(subtask.Data)
	if err == nil {
		return status.withJob(mergeData.JobID, func() error {
			logger = logger.WithJob(mergeData.JobID)
			if err := logger.LogStep("merge task", func() error {
				return handleMergeTask(driver, logger, mergeData)
			}); err != nil {
				return err
			}

			subtask.Data, err = serializeMergeData(mergeData)
			return err
		})
	}

	return errors.New("worker task format unrecognized")
}

func forEachDatum(driver driver.Driver, object *pfs.Object, cb func([]*common.Input) error) (retErr error) {
	getObjectClient, err := driver.PachClient().ObjectAPIClient.GetObject(driver.PachClient().Ctx(), object)
	if err != nil {
		return err
	}

	grpcReader := grpcutil.NewStreamingBytesReader(getObjectClient, nil)
	protoReader := pbutil.NewReader(grpcReader)

	allDatums := &DatumInputsList{}

	if err := protoReader.Read(allDatums); err != nil {
		return err
	}

	for _, datum := range allDatums.Datums {
		if err := cb(datum.Inputs); err != nil {
			return err
		}
	}

	return nil
}

func uploadRecoveredDatums(driver driver.Driver, logger logs.TaggedLogger, recoveredDatums []string, tag string) error {
	return logger.LogStep("uploading recovered datums", func() error {
		message := &RecoveredDatums{Hashes: recoveredDatums}

		buf := &bytes.Buffer{}
		pbw := pbutil.NewWriter(buf)
		if _, err := pbw.Write(message); err != nil {
			return nil
		}

		// TODO: may need to delete the tag first, supposedly this will fail if it already exists
		_, _, err := driver.PachClient().PutObject(buf, tag)
		return err
	})
}

func uploadChunk(driver driver.Driver, logger logs.TaggedLogger, cache *hashtree.MergeCache, tag string) error {
	return logger.LogStep("uploading hashtree chunk", func() error {
		// Merge the datums for this job into a chunk
		buf := &bytes.Buffer{}
		if err := cache.Merge(hashtree.NewWriter(buf), nil, nil); err != nil {
			return err
		}

		// Cache the hashtree in the chunk cache for this job
		cache, err := driver.ChunkCaches().GetOrCreateCache(logger.JobID())
		if err != nil {
			return err
		}

		if err := cache.Put(tag, bytes.NewBuffer(buf.Bytes())); err != nil {
			return err
		}

		// Upload the hashtree for this subtask to the given tag
		// TODO: may need to delete the tag first, supposedly this will fail if it already exists
		putObjectWriter, err := driver.PachClient().PutObjectAsync([]*pfs.Tag{client.NewTag(tag)})
		if err != nil {
			return err
		}

		if _, err := putObjectWriter.Write(buf.Bytes()); err != nil {
			return err
		}

		return putObjectWriter.Close()
	})
}

func checkS3Gateway(driver driver.Driver, logger logs.TaggedLogger) error {
	return backoff.RetryNotify(func() error {
		endpoint := fmt.Sprintf("http://%s:%s/",
			ppsutil.SidecarS3GatewayService(logger.JobID()),
			os.Getenv("S3GATEWAY_PORT"),
		)

		_, err := (&http.Client{Timeout: 5 * time.Second}).Get(endpoint)
		logger.Logf("checking s3 gateway service for job %q: %v", logger.JobID(), err)
		return err
	}, backoff.New60sBackOff(), func(err error, d time.Duration) error {
		logger.Logf("worker could not connect to s3 gateway for %q: %v", logger.JobID(), err)
		return nil
	})
	// TODO: `master` implementation fails the job here, we may need to do the same
	// We would need to load the jobInfo first for this:
	// }); err != nil {
	//   reason := fmt.Sprintf("could not connect to s3 gateway for %q: %v", logger.JobID(), err)
	//   logger.Logf("failing job with reason: %s", reason)
	//   // NOTE: this is the only place a worker will reach over and change the job state, this should not generally be done.
	//   return finishJob(driver.PipelineInfo(), driver.PachClient(), jobInfo, pps.JobState_JOB_FAILURE, reason, nil, nil, 0, nil, 0)
	// }
	// return nil
}

func handleDatumTask(driver driver.Driver, logger logs.TaggedLogger, data *DatumData, subtaskID string, status *Status) error {
	if ppsutil.ContainsS3Inputs(driver.PipelineInfo().Input) || driver.PipelineInfo().S3Out {
		if err := checkS3Gateway(driver, logger); err != nil {
			return err
		}
	}

	// TODO: check for existing tagged output files - continue with processing if any are missing
	return driver.WithDatumCache(func(datumCache *hashtree.MergeCache, statsCache *hashtree.MergeCache) error {
		logger.Logf("transform worker datum task: %v", data)
		limiter := limit.New(int(driver.PipelineInfo().MaxQueueSize))

		// statsMutex controls access to stats so that they can be safely merged
		statsMutex := &sync.Mutex{}
		recoveredDatums := []string{}
		data.Stats = &DatumStats{
			ProcessStats: &pps.ProcessStats{},
		}

		queueSize := int64(0)
		// TODO: the status.GetStatus call may read the process stats without having a lock, it this ~ok?
		if err := logger.LogStep("processing datums", func() error {
			return status.withStats(data.Stats.ProcessStats, &queueSize, func() error {
				ctx, cancel := context.WithCancel(driver.PachClient().Ctx())
				defer cancel()

				eg, ctx := errgroup.WithContext(ctx)
				driver := driver.WithContext(ctx)
				if err := forEachDatum(driver, data.Datums, func(inputs []*common.Input) error {
					limiter.Acquire()
					atomic.AddInt64(&queueSize, 1)
					eg.Go(func() error {
						defer limiter.Release()
						defer atomic.AddInt64(&queueSize, -1)

						// subStats is still valid even on an error, merge those in before proceeding
						subStats, subRecovered, err := processDatum(driver, logger.WithData(inputs), inputs, data.OutputCommit, datumCache, status)

						statsMutex.Lock()
						defer statsMutex.Unlock()
						statsErr := mergeStats(data.Stats, subStats)
						if err != nil {
							return err
						}
						recoveredDatums = append(recoveredDatums, subRecovered...)
						return statsErr
					})
					return nil
				}); err != nil {
					cancel()
					eg.Wait()
					return err
				}

				return eg.Wait()
			})
		}); err != nil {
			return err
		}

		if data.Stats.DatumsFailed == 0 && !driver.PipelineInfo().S3Out {
			if len(recoveredDatums) > 0 {
				recoveredDatumsTag := jobRecoveredDatumsTag(logger.JobID(), subtaskID)
				if err := uploadRecoveredDatums(driver, logger, recoveredDatums, recoveredDatumsTag); err != nil {
					return err
				}
				data.RecoveredDatumsTag = recoveredDatumsTag
			}

			chunkTag := jobChunkTag(logger.JobID(), subtaskID)
			if err := uploadChunk(driver, logger, datumCache, chunkTag); err != nil {
				return err
			}

			data.Hashtree = &HashtreeInfo{Address: os.Getenv(client.PPSWorkerIPEnv), Tag: chunkTag}
		}

		return nil
	})
}

func processDatum(
	driver driver.Driver,
	logger logs.TaggedLogger,
	inputs []*common.Input,
	outputCommit *pfs.Commit,
	datumCache *hashtree.MergeCache,
	status *Status,
) (*DatumStats, []string, error) {
	recoveredDatumTags := []string{}
	stats := &DatumStats{}
	tag := common.HashDatum(driver.PipelineInfo().Pipeline.Name, driver.PipelineInfo().Salt, inputs)
	datumID := common.DatumID(inputs)

	if _, err := driver.PachClient().InspectTag(driver.PachClient().Ctx(), client.NewTag(tag)); err == nil {
		logger.Logf("skipping datum")
		buf := &bytes.Buffer{}
		// TODO: should we count the size of 'buf' here towards downloaded data?
		if err := driver.PachClient().GetTag(tag, buf); err != nil {
			return stats, recoveredDatumTags, err
		}
		if err := datumCache.Put(uuid.NewWithoutDashes(), buf); err != nil {
			return stats, recoveredDatumTags, err
		}
		stats.DatumsSkipped++
		return stats, recoveredDatumTags, nil
	}

	var inputTree, outputTree *hashtree.Ordered
	/* TODO: enable stats
	var statsTree *hashtree.Unordered
	if driver.PipelineInfo().EnableStats {
		statsRoot := path.Join("/", datumID)
		inputTree = hashtree.NewOrdered(path.Join(statsRoot, "pfs"))
		outputTree = hashtree.NewOrdered(path.Join(statsRoot, "pfs", "out"))
		statsTree = hashtree.NewUnordered(statsRoot)
		// Write job id to stats tree
		statsTree.PutFile(fmt.Sprintf("job:%s", logger.JobID()), nil, 0)
		defer func() {
			if err := writeStats(driver, logger, tag, stats.ProcessStats, inputTree, outputTree, statsTree); err != nil && retErr == nil {
				retErr = err
			}
		}()
	}
	*/

	var failures int64
	if err := backoff.RetryUntilCancel(driver.PachClient().Ctx(), func() error {
		var err error

		// WithData will download the inputs for this datum
		stats.ProcessStats, err = driver.WithData(inputs, inputTree, logger, func(dir string, processStats *pps.ProcessStats) error {

			// WithActiveData acquires a mutex so that we don't run this section concurrently
			if err := driver.WithActiveData(inputs, dir, func() error {
				ctx, cancel := context.WithCancel(driver.PachClient().Ctx())
				defer cancel()

				driver := driver.WithContext(ctx)

				return status.withDatum(inputs, cancel, func() error {
					env := userCodeEnv(driver.PipelineInfo(), logger.JobID(), outputCommit, driver.InputDir(), inputs)
					if err := driver.RunUserCode(logger, env, processStats, driver.PipelineInfo().DatumTimeout); err != nil {
						if driver.PipelineInfo().Transform.ErrCmd != nil && failures == driver.PipelineInfo().DatumTries-1 {
							if err = driver.RunUserErrorHandlingCode(logger, env, processStats, driver.PipelineInfo().DatumTimeout); err != nil {
								return errors.Wrap(err, "RunUserErrorHandlingCode")
							}
							return errDatumRecovered
						}
						return err
					}
					return nil
				})
			}); err != nil {
				return err
			}

			if driver.PipelineInfo().S3Out {
				return nil // S3Out pipelines do not store data in worker hashtrees
			}

			hashtreeBytes, err := driver.UploadOutput(dir, tag, logger, inputs, processStats, outputTree)
			if err != nil {
				return err
			}

			// Cache datum hashtree locally
			return datumCache.Put(uuid.NewWithoutDashes(), bytes.NewReader(hashtreeBytes))
		})
		return err
	}, &backoff.ZeroBackOff{}, func(err error, d time.Duration) error {
		failures++
		if failures >= driver.PipelineInfo().DatumTries {
			logger.Logf("failed to process datum with error: %+v", err)
			/* TODO: enable stats
			if statsTree != nil {
				object, size, err := pachClient.PutObject(strings.NewReader(err.Error()))
				if err != nil {
					logger.Errf("could not put error object: %s\n", err)
				} else {
					objectInfo, err := pachClient.InspectObject(object.Hash)
					if err != nil {
						return err
					}
					h, err := pfs.DecodeHash(object.Hash)
					if err != nil {
						return err
					}
					statsTree.PutFile("failure", h, size, objectInfo.BlockRef)
				}
			}
			*/
			return err
		}
		logger.Logf("failed processing datum: %v, retrying in %v", err, d)
		return nil
	}); err == errDatumRecovered {
		// keep track of the recovered datums
		recoveredDatumTags = []string{tag}
		stats.DatumsRecovered++
	} else if err != nil {
		stats.FailedDatumID = datumID
		stats.DatumsFailed++
	} else {
		stats.DatumsProcessed++
	}
	fmt.Printf("datum processStats: %v\n", stats)
	return stats, recoveredDatumTags, nil
}

func userCodeEnv(
	pipelineInfo *pps.PipelineInfo,
	jobID string,
	outputCommit *pfs.Commit,
	inputDir string,
	inputs []*common.Input,
) []string {
	result := os.Environ()
	for _, input := range inputs {
		result = append(result, fmt.Sprintf("%s=%s", input.Name, filepath.Join(inputDir, input.Name, input.FileInfo.File.Path)))
		result = append(result, fmt.Sprintf("%s_COMMIT=%s", input.Name, input.FileInfo.File.Commit.ID))
	}
	result = append(result, fmt.Sprintf("%s=%s", client.JobIDEnv, jobID))
	result = append(result, fmt.Sprintf("%s=%s", client.OutputCommitIDEnv, outputCommit.ID))
	if ppsutil.ContainsS3Inputs(pipelineInfo.Input) || pipelineInfo.S3Out {
		// TODO(msteffen) Instead of reading S3GATEWAY_PORT directly, worker/main.go
		// should pass its ServiceEnv to worker.NewAPIServer, which should store it
		// in 'a'. However, requiring worker.APIServer to have a ServiceEnv would
		// break the worker.APIServer initialization in newTestAPIServer (in
		// worker/worker_test.go), which uses mock clients but has no good way to
		// mock a ServiceEnv. Once we can create mock ServiceEnvs, we should store
		// a ServiceEnv in worker.APIServer, rewrite newTestAPIServer and
		// NewAPIServer, and then change this code.
		result = append(
			result,
			fmt.Sprintf("S3_ENDPOINT=http://%s:%s",
				ppsutil.SidecarS3GatewayService(jobID),
				os.Getenv("S3GATEWAY_PORT"),
			),
		)
	}
	return result
}

/* TODO: enable stats
func writeStats(
	driver driver.Driver,
	logger logs.TaggedLogger,
	stats *pps.ProcessStats,
	inputTree *hashtree.Ordered,
	outputTree *hashtree.Ordered,
	statsTree *hashtree.Unordered,
	tag string,
) (retErr error) {
	// Store stats and add stats file
	marshaler := &jsonpb.Marshaler{}
	statsString, err := marshaler.MarshalToString(stats)
	if err != nil {
		logger.Errf("could not serialize stats: %s\n", err)
		return err
	}
	object, size, err := driver.PachClient().PutObject(strings.NewReader(statsString))
	if err != nil {
		logger.Errf("could not put stats object: %s\n", err)
		return err
	}
	objectInfo, err := driver.PachClient().InspectObject(object.Hash)
	if err != nil {
		return err
	}
	h, err := pfs.DecodeHash(object.Hash)
	if err != nil {
		return err
	}
	statsTree.PutFile("stats", h, size, objectInfo.BlockRef)
	// Store logs and add logs file
	object, size, err = logger.Close()
	if err != nil {
		return err
	}
	if object != nil {
		objectInfo, err := driver.PachClient().InspectObject(object.Hash)
		if err != nil {
			return err
		}
		h, err := pfs.DecodeHash(object.Hash)
		if err != nil {
			return err
		}
		statsTree.PutFile("logs", h, size, objectInfo.BlockRef)
	}
	// Merge stats trees (input, output, stats) and write out
	inputBuf := &bytes.Buffer{}
	inputTree.Serialize(inputBuf)
	outputBuf := &bytes.Buffer{}
	outputTree.Serialize(outputBuf)
	statsBuf := &bytes.Buffer{}
	statsTree.Ordered().Serialize(statsBuf)
	// Merge datum stats hashtree
	buf := &bytes.Buffer{}
	if err := hashtree.Merge(hashtree.NewWriter(buf), []*hashtree.Reader{
		hashtree.NewReader(inputBuf, nil),
		hashtree.NewReader(outputBuf, nil),
		hashtree.NewReader(statsBuf, nil),
	}); err != nil {
		return err
	}
	// Write datum stats hashtree to object storage
	objW, err := driver.PachClient().PutObjectAsync([]*pfs.Tag{client.NewTag(tag + statsTagSuffix)})
	if err != nil {
		return err
	}
	defer func() {
		if err := objW.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if _, err := objW.Write(buf.Bytes()); err != nil {
		return err
	}
	// Cache datum stats hashtree locally
	return driver.CacheStatsHashtree(logger.JobID(), tag, bytes.NewReader(buf.Bytes()))
}
*/

func fetchChunkFromWorker(driver driver.Driver, logger logs.TaggedLogger, address string, tag string, shard int64) (io.ReadCloser, error) {
	// TODO: cache cross-worker clients at the driver level
	client, err := server.NewClient(address)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(driver.PachClient().Ctx())
	getChunkClient, err := client.GetChunk(ctx, &server.GetChunkRequest{JobID: logger.JobID(), Tag: tag, Shard: shard})
	if err != nil {
		cancel()
		return nil, grpcutil.ScrubGRPC(err)
	}

	return grpcutil.NewStreamingBytesReader(getChunkClient, cancel), nil
}

func fetchChunk(driver driver.Driver, logger logs.TaggedLogger, info *HashtreeInfo, shard int64) (io.ReadCloser, error) {
	reader, err := fetchChunkFromWorker(driver, logger, info.Address, info.Tag, shard)
	if err == nil {
		return reader, nil
	}
	logger.Logf("error when fetching cached chunk (%s) from worker (%s) - fetching from object store instead: %v", info.Tag, info.Address, err)

	return driver.PachClient().GetTagReader(info.Tag)
}

func handleMergeTask(driver driver.Driver, logger logs.TaggedLogger, data *MergeData) (retErr error) {
	cache, err := driver.ChunkCaches().GetOrCreateCache(logger.JobID())
	if err != nil {
		return err
	}

	var parentReader io.ReadCloser
	defer func() {
		if parentReader != nil {
			if err := parentReader.Close(); retErr == nil {
				retErr = err
			}
		}
	}()

	if err := logger.LogStep("downloading hashtree chunks", func() error {
		eg, _ := errgroup.WithContext(driver.PachClient().Ctx())
		limiter := limit.New(20) // TODO: base this off of configuration

		cachedIDs := cache.Keys()
		usedIDs := make(map[string]struct{})

		for _, hashtreeInfo := range data.Hashtrees {
			usedIDs[hashtreeInfo.Tag] = struct{}{}

			if !cache.Has(hashtreeInfo.Tag) {
				limiter.Acquire()
				hashtreeInfo := hashtreeInfo
				eg.Go(func() (retErr error) {
					defer limiter.Release()
					reader, err := fetchChunk(driver, logger, hashtreeInfo, data.Shard)
					if err != nil {
						return err
					}

					defer func() {
						if err := reader.Close(); retErr == nil {
							retErr = err
						}
					}()

					// TODO: this only works if it is read into a buffer first?
					buf := &bytes.Buffer{}
					io.Copy(buf, reader)
					return cache.Put(hashtreeInfo.Tag, bytes.NewBuffer(buf.Bytes()))
				})
			}
		}

		// There may be cached trees from a failed run - drop them
		for _, id := range cachedIDs {
			if _, ok := usedIDs[id]; !ok {
				cache.Delete(id)
			}
		}

		if data.Parent != nil {
			eg.Go(func() error {
				var err error
				parentReader, err = driver.PachClient().GetObjectReader(data.Parent.Hash)
				return err
			})
		}

		return eg.Wait()
	}); err != nil {
		return err
	}

	return logger.LogStep("merging hashtree chunks", func() error {
		tree, size, err := merge(driver, parentReader, cache, data.Shard)
		if err != nil {
			return err
		}

		data.Tree = tree
		data.TreeSize = size
		return nil
	})
}

func merge(driver driver.Driver, parent io.Reader, cache *hashtree.MergeCache, shard int64) (*pfs.Object, uint64, error) {
	var tree *pfs.Object
	var size uint64
	if err := func() (retErr error) {
		objW, err := driver.PachClient().PutObjectAsync(nil)
		if err != nil {
			return err
		}

		w := hashtree.NewWriter(objW)
		filter := hashtree.NewFilter(driver.NumShards(), shard)
		err = cache.Merge(w, parent, filter)
		size = w.Size()
		if err != nil {
			objW.Close()
			return err
		}
		// Get object hash for hashtree
		if err := objW.Close(); err != nil {
			return err
		}
		tree, err = objW.Object()
		if err != nil {
			return err
		}
		// Get index and write it out
		indexData, err := w.Index()
		if err != nil {
			return err
		}
		return writeIndex(driver, tree, indexData)
	}(); err != nil {
		return nil, 0, err
	}
	return tree, size, nil
}

func writeIndex(driver driver.Driver, tree *pfs.Object, indexData []byte) (retErr error) {
	info, err := driver.PachClient().InspectObject(tree.Hash)
	if err != nil {
		return err
	}
	path, err := obj.BlockPathFromEnv(info.BlockRef.Block)
	if err != nil {
		return err
	}
	indexWriter, err := driver.PachClient().DirectObjWriter(path + hashtree.IndexPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := indexWriter.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	_, err = indexWriter.Write(indexData)
	return err
}
