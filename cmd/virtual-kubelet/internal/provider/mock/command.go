package mock

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
	"github.com/containerd/fifo"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"syscall"
)


var home_dir = os.Getenv("HOME")

func (p *MockProvider) collectScripts(ctx context.Context, pod *v1.Pod, vol map[string]string) {
	time_start := metav1.NewTime(time.Now())
	// define a map to store the bash scripts, as the key is the container name, the value is the list of bash scripts
	scripts := make(map[string][]string)
	for _, c := range pod.Spec.Containers {
		log.G(ctx).Infof("container name: %s", c.Name)
		scripts[c.Name] = []string{}
		for _, volMount := range c.VolumeMounts {
			workdir := vol[volMount.Name]
			mountdir := volMount.MountPath

			// if mountdir has ~, replace it with home_dir
			if strings.HasPrefix(mountdir, "~") {
				mountdir = strings.Replace(mountdir, "~", home_dir, 1)
			}

			log.G(ctx).Infof("volumeMount: %s, volume mount directory: %s", volMount.Name, mountdir)
			// if the volume mount is not found in the volume map, return error
			if workdir == "" {
				log.G(ctx).Infof("volumeMount %s not found in the volume map", volMount.Name)
				// update the container status to failed
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
					Name:         c.Name,
					Image:        c.Image,
					Ready:        false,
					RestartCount: 0,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							Message:    "volume mount not found in the volume map",
							FinishedAt: metav1.NewTime(time.Now()),
							Reason:     string(v1.PodFailed),
							StartedAt:  time_start,
						},
					},
				})
				continue
			}

			// run the command in the workdir
			//scan the workdir for bash scripts
			files, err := ioutil.ReadDir(workdir)
			if err != nil {
				log.G(ctx).Infof("failed to read workdir %s; error: %v", workdir, err)
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
					Name:         c.Name,
					Image:        c.Image,
					Ready:        false,
					RestartCount: 0,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							Message:    fmt.Sprintf("failed to read workdir %s; error: %v", workdir, err),
							FinishedAt: metav1.NewTime(time.Now()),
							Reason:     string(v1.PodFailed),
							StartedAt:  time_start,
						},
					},
				})
				continue
			}

			for _, f := range files {
				log.G(ctx).Infof("file name: %s", f.Name())
				// if f.Name() contains crt, key, or pem, skip it
				if strings.Contains(f.Name(), "crt") || strings.Contains(f.Name(), "key") || strings.Contains(f.Name(), "pem") {
					log.G(ctx).Infof("file name %s contains crt, key, or pem, skip it", f.Name())
					continue
				}

				// move f to the volume mount directory
				err := copyFile(ctx, workdir, mountdir, f.Name())
				if err != nil {
					pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
						Name:         c.Name,
						Image:        c.Image,
						Ready:        false,
						RestartCount: 0,
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								Message:    fmt.Sprintf("failed to copy file %s to %s; error: %v", path.Join(workdir, f.Name()), path.Join(mountdir, f.Name()), err),
								FinishedAt: metav1.NewTime(time.Now()),
								Reason:     string(v1.PodFailed),
								StartedAt:  time_start,
							},
						},
					})
					continue
				}

				script := path.Join(mountdir, f.Name())
				scripts[c.Name] = append(scripts[c.Name], script)
				// if strings.HasSuffix(f.Name(), ".job") {
				// 	script := path.Join(mountdir, f.Name())
				// 	log.G(ctx).Infof("found job script %s", script)
				// 	job_scripts[c.Name] = append(job_scripts[c.Name], script)
				// } else {
				// 	log.G(ctx).Infof("found non-job script %s", f.Name())
				// }
			}
		}
	}
	log.G(ctx).Infof("found scripts: %v", scripts)
}

// run container in parallel
func (p *MockProvider) runScriptParallel(ctx context.Context, pod *v1.Pod, vol map[string]string) (chan error, chan v1.ContainerStatus) {
	p.collectScripts(ctx, pod, vol)

	var wg sync.WaitGroup
	errChan := make(chan error, len(pod.Spec.Containers))
	cstatusChan := make(chan v1.ContainerStatus, len(pod.Spec.Containers))
	time_start := metav1.NewTime(time.Now())

	for _, c := range pod.Spec.Containers {
		var (
				pgid int = 0
				err error
				
			)
		wg.Add(1)
		go func(c v1.Container) {
			defer wg.Done()
			log.G(ctx).WithField("container", c.Name).Info("Starting container")

			//define command to run the bash script based on c.Command of list of strings
			var command = c.Command
			if len(command) == 0 {
				log.G(ctx).WithField("container", c.Name).Infof("No command found for container")
				err = fmt.Errorf("no command found for container: %s", c.Name)
				errChan <- err
				return
			}

			var args string
			if len(c.Args) == 0 {
				log.G(ctx).Infof("no args found for container %s", c.Name)
				err = fmt.Errorf("no args found for container %s", c.Name)
				errChan <- err
				return
			} else {
				args = strings.Join(c.Args, " ")
			}

			env := c.Env // what is the type of c.Env? []v1.EnvVar
			// if env contains fifo = true, write the command to the fifo
	
			// search the env for fifo = true
			runWithFifo := false
			for _, e := range env {
				if e.Name == "fifo" && e.Value == "true" {
					log.G(ctx).Infof("fifo env found for container %s", c.Name)
					runWithFifo = true					
					break
				}
			}

			if runWithFifo {
				log.G(ctx).Infof("fifo env found for container %s", c.Name)
				err = writeCmdToFifo(ctx, command, args, env)
			}else{
				log.G(ctx).Infof("fifo env not found for container %s", c.Name)
				args = strings.Replace(args, "~", home_dir, 1)
				pgid, err = runScript(ctx, command, args, env)
			}


			// print pod.status.containerStatuses
			if err != nil {
				// report error to errChan
				errChan <- err
				// report container status to container_status
				cstatusChan <- v1.ContainerStatus{
					Name:         c.Name,
					Image:        c.Image,
					Ready:        false,
					RestartCount: 0,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							Message:    fmt.Sprintf("failed to run cmd %s, arg %s; error: %v", command, args, err),
							FinishedAt: metav1.NewTime(time.Now()),
							Reason:     string(v1.PodFailed),
							StartedAt:  time_start,
						},
					},
				}
				return
			}

			//write the leader pid to the leader_pid file
			// name leader_pid file as container name + .leader_pid
			if !runWithFifo {
				var pgid_volmount string
				for _, volMount := range c.VolumeMounts {
					if strings.Contains(volMount.Name, "pgid") {
						pgid_volmount = volMount.MountPath
					}
				}

				if pgid_volmount == "" {
					log.G(ctx).Infof("pgid volume mount not found for container %s", c.Name)
					err = fmt.Errorf("pgid volume mount not found for container %s", c.Name)
					errChan <- err
					return
				}

				pgidFile := path.Join(c.Name + ".pgid")
				err = writePgid(ctx, pgid_volmount, pgidFile, pgid)
				if err != nil {
					//report error to errChan
					errChan <- err
					// report container status to container_status
					cstatusChan <- v1.ContainerStatus{
						Name:         c.Name,
						Image:        c.Image,
						Ready:        false,
						RestartCount: 0,
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								Message:    fmt.Sprintf("failed to write pgid to file %s; error: %v", pgidFile, err),
								FinishedAt: metav1.NewTime(time.Now()),
								Reason:     string(v1.PodFailed),
								StartedAt:  time_start,
							},
						},
					}
					return
				}
			}


			// update the container status to success
			cstatusChan <- v1.ContainerStatus{
				Name:         c.Name,
				Image:        c.Image,
				Ready:        true,
				RestartCount: 0,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						Message:    fmt.Sprintf("container %s executed successfully", c.Name),
						Reason:     string(v1.PodSucceeded),
						FinishedAt: metav1.NewTime(time.Now()),
						StartedAt:  time_start,
					},
				},
			}
		}(c)
	}

	go func() {
		wg.Wait()
		close(errChan)
		close(cstatusChan) // close the channel
	}()

	return errChan, cstatusChan

	// // update container status based on the output of the goroutines above
	// for err := range errChan {
	// 	if err != nil {
	// 		log.G(ctx).Infof("error: %v", err)
	// 		// update the container status to failed
	// 	}
	// }

	// for cstatus := range cstatusChan {
	// 	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, cstatus)
	// 	log.G(ctx).Infof("container status: %v", cstatus)
	// }
}

func writePgid(ctx context.Context, volmount string, file string, pgid int) error {
	volmount = strings.Replace(volmount, "~", home_dir, 1)
	// create the destination directory if it does not exist
	err := exec.Command("mkdir", "-p", volmount).Run()
	if err != nil {
		log.G(ctx).WithField("volmount", volmount).Errorf("failed to create directory; error: %v", err)
		return err
	}

	//write the leader pid to the leader_pid file
	// name leader_pid file as container name + .leader_pid
	file = path.Join(volmount, file)
	err = ioutil.WriteFile(file, []byte(fmt.Sprintf("%v", pgid)), 0644)
	if err != nil {
		log.G(ctx).Infof("failed to write pgid to file %s; error: %v", file, err)
		return err
	}
	log.G(ctx).Infof("successfully wrote pgid to file %s", file)
	return nil
}

func runScript(ctx context.Context, command []string, args string, env []v1.EnvVar) (int, error) {
	// run the script with the env variables set
	// run the command like [command[0], command[1], ...] args
	
	// command = append(command, args)
	// cmd := exec.Command(command[0], command[1:]...)


	cmdString := strings.Join(command, " ")
    cmd := cmdString + " '"+ args + "'"
	cmd = cmd + fmt.Sprintf(" >> %s/stdout 2>> %s/stderr", os.Getenv("HOME"), os.Getenv("HOME"))

	cmd = strings.Replace(cmd, "~", os.Getenv("HOME"), 1)
	cmd2 := exec.Command("/bin/bash", "-c", cmd)


	cmd2.Env = os.Environ()
	for _, e := range env {
		log.G(ctx).Infof("env name: %s, env value: %s", e.Name, e.Value)
		cmd2.Env = append(cmd2.Env, fmt.Sprintf("%s=%s", e.Name, e.Value))
	}

	// var out bytes.Buffer
	// var stderr bytes.Buffer
	// cmd.Stdout = &out
	// cmd.Stderr = &stderr

	log.G(ctx).Infof("start running command: %s, arg: %s", command, args)

	err := cmd2.Start()
	if err != nil {
		log.G(ctx).Infof("failed to run the cmd. error: %v", err)
		return 0, err
	}

	pgid, err := syscall.Getpgid(cmd2.Process.Pid)
	if err != nil {
		log.G(ctx).Infof("failed to get pgid. error: %v", err)
		return 0, err
	}

	log.G(ctx).Infof("successfully ran cmd pgid: %v", pgid)
	return pgid, nil
}


func writeCmdToFifo(ctx context.Context, command []string, args string, env []v1.EnvVar) error {
	homeDir := os.Getenv("HOME")
	fifoPath := homeDir + "/hostpipe"
    fn := fifoPath + "/vk-cmd"
    flag := syscall.O_WRONLY 
    perm := os.FileMode(0666)

    fifo, err := fifo.OpenFifo(ctx, fn, flag, perm)
    if err != nil {
		log.G(ctx).Infof("failed to open fifo %s; error: %v\n", fn, err)
        return err
    }

    // write env to a single string like "export key1='value1'&& export key2='value2'..."
    var envString string
    for _, e := range env {
        // if type of value is string, use single quotes to prevent shell from interpreting the value
		envString += "export " + e.Name + "=\"" + e.Value + "\" && "
        //if type of value is int or float, no quotes are needed
    }


    //use single quotes to around the argsString to prevent shell from interpreting the args
	cmdString := strings.Join(command, " ")
    cmd := cmdString + " '" + envString + args + "'"

	log.G(ctx).Infof("Running cmd: %s\n", cmd)

    _, err = fifo.Write([]byte(cmd))
    if err != nil {
		log.G(ctx).Infof("failed to write to fifo %s; error: %v\n", fn, err)
        return err
    }

    return nil
}




func copyFile(ctx context.Context, src string, dst string, filename string) error {
	// create the destination directory if it does not exist
	err := exec.Command("mkdir", "-p", dst).Run()
	if err != nil {
		log.G(ctx).Infof("failed to create directory %s; error: %v", dst, err)
		return err
	}
	//mv the file to the destination directory
	err = exec.Command("cp", path.Join(src, filename), path.Join(dst, filename)).Run()
	if err != nil {
		log.G(ctx).Infof("failed to copy file %s to %s; error: %v", path.Join(src, filename), path.Join(dst, filename), err)
		return err
	}
	return nil
}

// func (p *MockProvider) runBashScript(ctx context.Context, pod *v1.Pod, vol map[string]string) {
// 	for _, c := range pod.Spec.Containers {
// 		start_container := metav1.NewTime(time.Now())
// 		for _, volMount := range c.VolumeMounts {
// 			workdir := vol[volMount.Name]
// 			log.G(ctx).Infof("volume mount directory: %s", workdir)

// 			// if the volume mount is not found in the volume map, return error
// 			if workdir == "" {
// 				log.G(ctx).Infof("volume mount %s not found in the volume map", volMount.Name)
// 				// update the container status to failed
// 				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
// 					Name:         c.Name,
// 					Image:        c.Image,
// 					Ready:        false,
// 					RestartCount: 0,
// 					State: v1.ContainerState{
// 						Terminated: &v1.ContainerStateTerminated{
// 							Message:    "volume mount not found in the volume map",
// 							FinishedAt: metav1.NewTime(time.Now()),
// 							Reason:     "VolumeMountNotFound",
// 							StartedAt:  start_container,
// 						},
// 					},
// 				})
// 				break
// 			}

// 			// run the command in the workdir
// 			//scan the workdir for bash scripts
// 			files, err := ioutil.ReadDir(workdir)
// 			if err != nil {
// 				log.G(ctx).Infof("failed to read workdir %s; error: %v", workdir, err)
// 				// update the container status to failed
// 				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
// 					Name:         c.Name,
// 					Image:        c.Image,
// 					Ready:        false,
// 					RestartCount: 0,
// 					State: v1.ContainerState{
// 						Terminated: &v1.ContainerStateTerminated{
// 							Message:    fmt.Sprintf("failed to read workdir %s; error: %v", workdir, err),
// 							FinishedAt: metav1.NewTime(time.Now()),
// 							Reason:     "WorkdirReadFailed",
// 							StartedAt:  start_container,
// 						},
// 					},
// 				})
// 				break
// 			}

// 			for _, f := range files {
// 				start_running := metav1.NewTime(time.Now())
// 				if strings.HasSuffix(f.Name(), ".job") {
// 					script := path.Join(workdir, f.Name())
// 					log.G(ctx).Infof("running bash script %s", script)

// 					// run the bash script in the workdir
// 					leader_pid, err := executeProcess(ctx, script)
// 					if err != nil {
// 						log.G(ctx).Infof("failed to run bash script: %s; error: %v", script, err)
// 					}
// 					log.G(ctx).Infof("Leader pid: %v", leader_pid)

// 					if err != nil {
// 						log.G(ctx).Infof("failed to run bash script: %s; error: %v", script, err)
// 						// update the container status to failed
// 						pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
// 							Name:         c.Name,
// 							Image:        c.Image,
// 							Ready:        false,
// 							RestartCount: 0,
// 							State: v1.ContainerState{
// 								Terminated: &v1.ContainerStateTerminated{
// 									Message:    fmt.Sprintf("failed to run bash script: %s; error: %v", script, err),
// 									Reason:     "BashScriptFailed",
// 									StartedAt:  start_running,
// 								},
// 							},
// 						})
// 						break
// 					}

// 					if err != nil {
// 						log.G(ctx).Infof("failed to run bash script: %s; error: %v", script, err)
// 						// update the container status to failed
// 						pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
// 							Name:         c.Name,
// 							Image:        c.Image,
// 							Ready:        false,
// 							RestartCount: 0,
// 							State: v1.ContainerState{
// 								Terminated: &v1.ContainerStateTerminated{
// 									Message:    fmt.Sprintf("failed to run bash script: %s; error: %v", script, err),
// 									Reason:     "BashScriptFailed",
// 									StartedAt:  start_running,
// 								},
// 							},
// 						})
// 						break
// 					}

// 					log.G(ctx).Infof("bash script executed successfully in workdir %s", script)
// 					// update the container status to success
// 					pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
// 						Name:         c.Name,
// 						Image:        c.Image,
// 						Ready:        true,
// 						RestartCount: 0,
// 						State: v1.ContainerState{
// 							Terminated: &v1.ContainerStateTerminated{
// 								Message:    fmt.Sprintf("bash script executed successfully in workdir %s", script),
// 								Reason:     "BashScriptSuccess",
// 								StartedAt:  start_running,
// 							},
// 						},
// 					})

// 					sleep := time.Duration(10) * time.Second
// 					time.Sleep(sleep)
// 				}
// 			}
// 		}
// 	}
// }

// // run the bash script in the workdir and keep track of the pids of the processes and their children
// func executeProcess(ctx context.Context, script string) (int, error) {
// 	// dont wait for the script to finish
// 	cmd := exec.CommandContext(ctx, "bash", script) // run the bash script in the workdir without waiting for it to finish
// 	err := cmd.Start()

// 	if err != nil {
// 		return 0, err
// 	}

// 	leader_pid := cmd.Process.Pid
// 	return leader_pid, nil
// }
