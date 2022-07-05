# Architecture Diagrams

## How to Include Diagrams

1. Include a `.py` file named after your feature that contains the contents of the architecture diagram. See [Diagrams documentation](https://diagrams.mingrammer.com/) for how to create diagrams as code.
2. Run `make build` to build the docker image that includes graphviz and related python dependencies.
3. Run `make run` to create all of the diagrams. The output diagrams will be saved to the `diagrams/` folder and can be referenced directly in the project `README`.

## Notes
1. To build locally without docker, first setup a venv with
```
python -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```
2. Next, you can build a diagram with target
```
make runlocal/filename.py
```
where filename.py is the corresponding diagrams-as-code python file.