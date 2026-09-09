ARG PYTHON_IMAGE=python:3.13-slim-bookworm@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e

FROM ${PYTHON_IMAGE} AS build

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_ROOT_USER_ACTION=ignore

RUN python -m venv /opt/venv
ENV PATH=/opt/venv/bin:${PATH}

WORKDIR /src
COPY trainer/pyproject.toml ./pyproject.toml
COPY trainer/belady_trainer ./belady_trainer
COPY trainer/belady ./belady
RUN pip install .

FROM ${PYTHON_IMAGE}

RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin nonroot

COPY --from=build /opt/venv /opt/venv

ENV PATH=/opt/venv/bin:${PATH} \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

USER 65532:65532
WORKDIR /work

ENTRYPOINT ["python", "-m", "belady_trainer"]
CMD ["train"]
