FROM postgres:16-alpine
COPY entrypoint-replica.sh /usr/local/bin/entrypoint-replica.sh
RUN chmod +x /usr/local/bin/entrypoint-replica.sh
ENTRYPOINT ["entrypoint-replica.sh"]
CMD ["postgres"]
