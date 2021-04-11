Package inotify implements a wrapper for the Linux inotify system.

Example:

    watcher, err := inotify.NewWatcher()
        if err != nil {
            log.Fatal(err)
        }
        defer watcher.Close()
        err = watcher.Add("/tmp/foo")
        if err != nil {
            log.Fatal(err)
        }
        select {
            case event, ok := <-watcher.Events:
                if !ok {
                    return
                }
                log.Println("event:", event)
                if event.Op&inotify.Write == inotify.Write {
                    log.Println("modified file:", event.Name)
                }
            case err, ok := <-watcher.Errors:
                if !ok {
                    return
                }
                log.Println("error:", err)
        }

Forked from github.com/fsnotify/fsnotify