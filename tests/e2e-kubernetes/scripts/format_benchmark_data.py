#!/usr/bin/python3

import sys
import json

if len(sys.argv) != 3:
    print("usage: ./script.py <input_file_path> <output_file_path>")

input_file_path = sys.argv[1]
output_file_path = sys.argv[2]

quicksight_json = []
with open(input_file_path) as f:
    data = json.load(f)

    for entry in data["entries"]["Benchmark"]:
        new_entry = {}
        new_entry["commit_id"] = entry["commit"]["id"]
        for bench in entry["benches"]:
            new_entry[bench["name"]] = bench["value"]
        quicksight_json.append(new_entry)

with open(output_file_path, "w") as f:
    f.write(json.dumps(quicksight_json))
