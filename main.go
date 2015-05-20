// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Peter Mattis (peter.mattis@gmail.com)

package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

func prettyJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func cacheDir() string {
	d := os.Getenv("CACHE")
	if d == "" {
		d = os.ExpandEnv("${HOME}/buildcache")
	}
	return d
}

func linkOrCopy(src, dst string) error {
	if exists(dst) {
		return nil
	}
	if err := os.Link(src, dst); err == nil || os.IsExist(err) {
		return nil
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()
	if err := dstFile.Chmod(srcInfo.Mode() & os.ModePerm); err != nil {
		_ = os.Remove(dst)
		return err
	}
	_, err = io.Copy(dstFile, srcFile)
	return err
}

func save(args []string) {
	if len(args) == 0 {
		args = []string{"."}
	}

	dir := cacheDir()
	log.Printf("saving %s to %s", args, dir)
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}

	start := time.Now()
	pkgs := loadAll(args)
	log.Printf("finished loading: %s", time.Since(start))

	for _, pkg := range pkgs {
		if pkg.Standard && !pkg.race {
			continue
		}
		if pkg.Stale || !exists(pkg.Target) {
			log.Printf("%-40s  %s (%s)", "-", pkg.ImportPath, pkg.Target)
		} else {
			fp := pkg.Fingerprint()
			tag := "*"
			dst := filepath.Join(dir, fp)
			if exists(dst) {
				tag = " "
			} else if err := linkOrCopy(pkg.Target, dst); err != nil {
				log.Fatal(err)
			}
			log.Printf("%-40s %s%s (%s)", fp, tag, pkg.ImportPath, pkg.Target)
		}
	}
}

func restore(args []string) {
	if len(args) == 0 {
		args = []string{"."}
	}

	dir := cacheDir()
	if !exists(dir) {
		log.Printf("%s does not exist", dir)
		os.Exit(0)
	}
	log.Printf("restoring %s from %s", args, dir)

	start := time.Now()
	pkgs := loadAll(args)
	log.Printf("finished loading: %s", time.Since(start))

	now := time.Now()
	for _, pkg := range pkgs {
		if pkg.Standard && !pkg.race {
			continue
		}
		fp := pkg.Fingerprint()
		src := filepath.Join(dir, fp)
		if !exists(src) {
			log.Printf("%-40s  %s (%s:%s)", "-", pkg.ImportPath, fp, pkg.Target)
		} else {
			log.Printf("%-40s  %s (%s)", fp, pkg.ImportPath, pkg.Target)
			_ = os.Remove(pkg.Target)
			_ = os.MkdirAll(filepath.Dir(pkg.Target), 0755)
			if err := linkOrCopy(src, pkg.Target); err != nil {
				log.Fatal(err)
			}
			if err := os.Chtimes(pkg.Target, now, now); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func clear(args []string) {
	// TODO(pmattis): Instead of removing everything, only clear entries
	// that are older than a day or week.
	dir := cacheDir()
	log.Printf("clearing %s", dir)
	if err := os.RemoveAll(dir); err != nil {
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(0)

	flag.Parse()
	args := flag.Args()

	if len(args) >= 1 {
		switch args[0] {
		case "save":
			save(args[1:])
			return
		case "restore":
			restore(args[1:])
			return
		case "clear":
			clear(args[1:])
			return
		}
		log.Printf("unknown command \"%s\"\n\n", args[0])
	}

	log.Printf("usage: %s [save|restore|clear]", os.Args[0])
	os.Exit(1)
}
