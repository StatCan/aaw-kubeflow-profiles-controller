# Architecture Diagrams

## How to Include Diagrams

1. Include a `.py` file named after your feature that contains the contents of the architecture diagram. See [Diagrams documentation](https://diagrams.mingrammer.com/) for how to create diagrams as code.
2. Run `make build` to build the docker image that includes graphviz and related python dependencies.
3. Run `make run` to create all of the diagrams. The output diagrams will be saved to the `diagrams/` folder and can be referenced directly in the project `README`.