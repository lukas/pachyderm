package pretty

import (
	"fmt"
	"io"

	"github.com/fatih/color"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
)

func PrintJobHeader(w io.Writer) {
	fmt.Fprint(w, "ID\tOUTPUT\tSTATE\t\n")
}

func PrintJobInfo(w io.Writer, jobInfo *ppsclient.JobInfo) {
	fmt.Fprintf(w, "%s\t", jobInfo.Job.ID)
	if jobInfo.OutputCommit != nil {
		fmt.Fprintf(w, "%s/%s\t", jobInfo.OutputCommit.Repo.Name, jobInfo.OutputCommit.ID)
	} else {
		fmt.Fprintf(w, "-\t")
	}
	fmt.Fprintf(w, "%s\t\n", jobState(jobInfo))
}

func PrintPipelineHeader(w io.Writer) {
	fmt.Fprint(w, "NAME\tINPUT\tOUTPUT\t\n")
}

func PrintPipelineInfo(w io.Writer, pipelineInfo *ppsclient.PipelineInfo) {
	fmt.Fprintf(w, "%s\t", pipelineInfo.Pipeline.Name)
	if len(pipelineInfo.Inputs) == 0 {
		fmt.Fprintf(w, "\t")
	} else {
		for i, input := range pipelineInfo.Inputs {
			fmt.Fprintf(w, "%s", input.Repo.Name)
			if i == len(pipelineInfo.Inputs)-1 {
				fmt.Fprintf(w, "\t")
			} else {
				fmt.Fprintf(w, ", ")
			}
		}
	}
	if pipelineInfo.OutputRepo != nil {
		fmt.Fprintf(w, "%s\t\n", pipelineInfo.OutputRepo.Name)
	} else {
		fmt.Fprintf(w, "\t\n")
	}
}

func jobState(jobInfo *ppsclient.JobInfo) string {
	switch jobInfo.State {
	case ppsclient.JobState_JOB_STATE_RUNNING:
		return color.New(color.FgYellow).SprintFunc()("running")
	case ppsclient.JobState_JOB_STATE_FAILURE:
		return color.New(color.FgRed).SprintFunc()("failure")
	case ppsclient.JobState_JOB_STATE_SUCCESS:
		return color.New(color.FgGreen).SprintFunc()("success")
	}
	return "-"
}
