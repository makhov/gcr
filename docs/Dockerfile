# The documentation site's toolchain, for make docs-serve-dev: mkdocs
# with the versions CI builds the site with (requirements.txt).
FROM python:3.13-slim
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --disable-pip-version-check -r /tmp/requirements.txt
WORKDIR /gcr
EXPOSE 8000
CMD ["mkdocs", "serve", "--dev-addr", "0.0.0.0:8000"]
