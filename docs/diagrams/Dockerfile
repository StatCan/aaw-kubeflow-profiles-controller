FROM python:3-alpine

RUN apk add \
      graphviz \
      font-noto \
      font-noto-emoji


# add new user
ARG USER=diagrams
RUN adduser -D $USER
USER $USER

WORKDIR /tmp
COPY requirements.txt .
RUN pip install -r requirements.txt
# docker run -v $(pwd):/tmp/ <image> diagram.py
ENTRYPOINT ["python"]