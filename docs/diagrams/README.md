# Architecture Diagrams

## How to Include Diagrams
1. To build locally, first setup a venv with
```
python -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```
1. Include a `.py` file named after your feature that contains the contents of the architecture diagram. See [Diagrams documentation](https://diagrams.mingrammer.com/) for how to create diagrams as code.
2. Run `make runlocal/<filename>.py` to create the diagram for your file `<filename>`. The output diagrams will be saved to the `diagrams/` folder and can be referenced directly in the project `README`.

## Development Notes
1. You can build a diagram with target
```
make runlocal/<filename>.py
```
where `<filename>.py` is the corresponding diagrams-as-code python file.

2. For rapid development, run
```
make devlocal/<filename>.py
```
The makefile will run in your terminal, watching for changes to the .py file and re-build the diagram for you.

## Dependencies
- graphviz: `sudo apt install graphviz`
- inotify: `sudo apt install inotify-tools`