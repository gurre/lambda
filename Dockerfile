FROM golang:1.24 as build
WORKDIR /testlambda
# Copy dependencies list
COPY . .
RUN go build -o bootstrap ./cmd/bootstrap/main.go
# Copy artifacts to a clean image
FROM public.ecr.aws/lambda/provided:al2023
COPY --from=build /testlambda/bootstrap ./bootstrap
ENTRYPOINT [ "./bootstrap" ]